package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() {
	logOut = io.Discard
}

func writeConfigFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func selfSignedCert(t *testing.T, domains []string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domains[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     domains,
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func recordingServer(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, tag+":"+request.URL.Path+"|host="+request.Host)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func startProxy(t *testing.T, configPath string, tlsConfig *tls.Config) (string, *proxy) {
	return startProxyWithNameservers(t, configPath, tlsConfig, testNameservers(t))
}

func startProxyWithNameservers(t *testing.T, configPath string, tlsConfig *tls.Config, servers *nameserverSet) (string, *proxy) {
	t.Helper()
	transport := newTransport(servers.DialContext)
	p, err := newProxy(configPath, transport, tlsConfig, nil, servers)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() { _ = serve(listener, tlsConfig, p) }()
	return "http://" + listener.Addr().String(), p
}

func testNameservers(t *testing.T, entries ...string) *nameserverSet {
	t.Helper()
	servers, err := newNameserverSet(2*time.Second, entries)
	if err != nil {
		t.Fatalf("newNameserverSet: %v", err)
	}
	return servers
}

func proxyClient(proxyURL string) *http.Client {
	parsed, _ := url.Parse(proxyURL)
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(parsed),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

func requestBody(t *testing.T, client *http.Client, rawURL string) string {
	t.Helper()
	resp, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(body))
}

func TestLoadTable_Valid(t *testing.T) {
	path := writeConfigFile(t, "r.yaml", `
servers:
  - domain: a.example.com
    routes:
      - prefix: /v1/
        upstream: http://up-a/v1/
      - prefix: /
        upstream: http://up-a
  - domain: b.example.com
    routes:
      - prefix: /
        upstream: https://up-b
`)
	table, _, err := loadTable(path)
	if err != nil {
		t.Fatalf("loadTable: %v", err)
	}
	if len(table.byDomain) != 2 {
		t.Fatalf("want 2 domains, got %d", len(table.byDomain))
	}
	entry, ok := table.pick("a.example.com", "/v1/x")
	if !ok || entry.prefix != "/v1/" {
		t.Fatalf("pick /v1/: got %+v ok=%v", entry, ok)
	}
	if _, ok := table.pick("b.example.com", "/"); !ok {
		t.Fatalf("pick b.example.com: not found")
	}
}

func TestLoadTable_Errors(t *testing.T) {
	cases := map[string]string{
		"empty_domain": "servers:\n  - domain: \"\"\n    routes: []",
		"dup_domain":   "servers:\n  - domain: a\n    routes: []\n  - domain: a\n    routes: []",
		"empty_prefix": "servers:\n  - domain: a\n    routes:\n      - prefix: \"\"\n        upstream: http://x",
		"dup_prefix":   "servers:\n  - domain: a\n    routes:\n      - prefix: /a\n        upstream: http://x\n      - prefix: /a\n        upstream: http://y",
		"bad_upstream": "servers:\n  - domain: a\n    routes:\n      - prefix: /\n        upstream: ftp://x",
		"bad_prefix":   "servers:\n  - domain: a\n    routes:\n      - prefix: v1\n        upstream: http://x",
		"bad_yaml":     "servers: [this is broken",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfigFile(t, name+".yaml", content)
			if _, _, err := loadTable(path); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestLoadTable_LocalPathUpstream(t *testing.T) {
	cases := map[string]string{
		"relative": "./dist",
		"unix_abs": "/var/www",
		"win_back": `C:\Users\me\dist`,
		"win_fwd":  "D:/web/dist",
		"unc":      `\\server\share`,
	}
	for name, up := range cases {
		t.Run(name, func(t *testing.T) {
			content := "servers:\n  - domain: a.example.com\n    routes:\n      - prefix: /\n        upstream: " + up + "\n"
			path := writeConfigFile(t, name+".yaml", content)
			table, _, err := loadTable(path)
			if err != nil {
				t.Fatalf("loadTable upstream=%q: unexpected error: %v", up, err)
			}
			entry, ok := table.pick("a.example.com", "/")
			if !ok {
				t.Fatalf("route not found for upstream %q", up)
			}
			if entry.fileRoot != up {
				t.Fatalf("fileRoot: got %q want %q", entry.fileRoot, up)
			}
			if entry.target != nil {
				t.Fatalf("target should be nil for local path %q", up)
			}
		})
	}
}

func TestPick_LongestPrefix(t *testing.T) {
	table := &routeTable{byDomain: map[string][]*route{
		"a": {
			{prefix: "/"},
			{prefix: "/v1/"},
			{prefix: "/v1/users/"},
			{prefix: "/api"},
		},
	}}
	cases := map[string]string{
		"/v1/users/1": "/v1/users/",
		"/v1/other":   "/v1/",
		"/other":      "/",
		"/api":        "/api",
		"/api/x":      "/api",
		"/api-v2":     "/",
	}
	for path, want := range cases {
		entry, ok := table.pick("a", path)
		if !ok || entry.prefix != want {
			t.Fatalf("pick %s: want %s got %+v ok=%v", path, want, entry, ok)
		}
	}
	if _, ok := table.pick("other", "/"); ok {
		t.Fatal("unknown domain should not match")
	}
}

func TestJoinPath(t *testing.T) {
	cases := []struct {
		base, rest, want string
	}{
		{"", "", "/"},
		{"", "foo", "/foo"},
		{"", "/foo", "/foo"},
		{"/v1/", "users", "/v1/users"},
		{"/v1", "/users", "/v1/users"},
		{"/v1/", "/users", "/v1/users"},
		{"/v1", "", "/v1/"},
		{"/", "", "/"},
		{"/", "foo", "/foo"},
	}
	for _, tc := range cases {
		if got := joinPath(tc.base, tc.rest); got != tc.want {
			t.Fatalf("joinPath(%q, %q) = %q, want %q", tc.base, tc.rest, got, tc.want)
		}
	}
}

func TestLoadTable_NormalizesDomainCase(t *testing.T) {
	path := writeConfigFile(t, "r.yaml",
		"servers:\n  - domain: API.Example.COM\n    routes:\n      - prefix: /\n        upstream: http://up-a\n")
	table, _, err := loadTable(path)
	if err != nil {
		t.Fatalf("loadTable: %v", err)
	}
	if !table.has("api.example.com") {
		t.Fatal("domain should be normalized to lowercase")
	}
}

func TestLoadTable_TrimsWhitespace(t *testing.T) {
	path := writeConfigFile(t, "r.yaml",
		"servers:\n  - domain: \" api.example.com \"\n    routes:\n      - prefix: \" /v1/ \"\n        upstream: http://up-a\n        host: \" up-a.example.com \"\n")
	table, _, err := loadTable(path)
	if err != nil {
		t.Fatalf("loadTable: %v", err)
	}
	if !table.has("api.example.com") {
		t.Fatal("domain should be trimmed")
	}
	entry, ok := table.pick("api.example.com", "/v1/x")
	if !ok || entry.prefix != "/v1/" {
		t.Fatalf("prefix should be trimmed: got %+v ok=%v", entry, ok)
	}
	if entry.host != "up-a.example.com" {
		t.Fatalf("host should be trimmed: got %q", entry.host)
	}
}

func TestHostOnly_Lowercase(t *testing.T) {
	if got := hostOnly("API.Example.COM:443"); got != "api.example.com" {
		t.Fatalf("hostOnly: got %q", got)
	}
	if got := hostOnly("API.Example.COM"); got != "api.example.com" {
		t.Fatalf("hostOnly bare: got %q", got)
	}
}

func TestLoadTLSConfig_RejectsNonCA(t *testing.T) {
	dir := t.TempDir()
	cert := selfSignedCert(t, []string{"api.example.com"})
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o644); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadTLSConfig(certPath, keyPath); err == nil {
		t.Fatal("want error for non-CA certificate")
	}
}

func TestProxy_HTTPRoutingAndPassthrough(t *testing.T) {
	upA := recordingServer(t, "A")
	upB := recordingServer(t, "B")
	content := "servers:\n" +
		"  - domain: api.example.com\n" +
		"    routes:\n" +
		"      - prefix: /v1/\n" +
		"        upstream: " + upA.URL + "/v1/\n" +
		"      - prefix: /api/\n" +
		"        upstream: " + upB.URL + "\n" +
		"        host: override.example.com\n"
	cfg := writeConfigFile(t, "r.yaml", content)
	proxyURL, _ := startProxy(t, cfg, nil)
	client := proxyClient(proxyURL)

	if got := requestBody(t, client, "http://api.example.com/v1/users"); !strings.HasPrefix(got, "A:/v1/users") {
		t.Fatalf("prefix mapping: got %q", got)
	}
	got := requestBody(t, client, "http://api.example.com/api/x?q=1")
	if !strings.HasPrefix(got, "B:/x") || !strings.Contains(got, "host=override.example.com") {
		t.Fatalf("host override: got %q", got)
	}
	if got := requestBody(t, client, upA.URL+"/misc"); !strings.HasPrefix(got, "A:/misc") {
		t.Fatalf("passthrough: got %q", got)
	}
}

func TestProxy_StaticDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<h1>home</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := writeConfigFile(t, "r.yaml",
		"servers:\n  - domain: static.example.com\n    routes:\n      - prefix: /\n        upstream: "+root+"\n")
	proxyURL, _ := startProxy(t, cfg, nil)
	client := proxyClient(proxyURL)

	if got := requestBody(t, client, "http://static.example.com/"); got != "<h1>home</h1>" {
		t.Fatalf("index: got %q", got)
	}
	if got := requestBody(t, client, "http://static.example.com/assets/app.js"); got != "console.log(1)" {
		t.Fatalf("subfile: got %q", got)
	}
	resp, err := client.Get("http://static.example.com/empty")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("empty dir without index: want 404, got %d", resp.StatusCode)
	}
	resp, err = client.Get("http://static.example.com/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing file: want 404, got %d", resp.StatusCode)
	}
}

func TestProxy_StaticPrefixMapping(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := writeConfigFile(t, "r.yaml",
		"servers:\n  - domain: assets.example.com\n    routes:\n      - prefix: /assets/\n        upstream: "+root+"\n")
	proxyURL, _ := startProxy(t, cfg, nil)
	client := proxyClient(proxyURL)

	if got := requestBody(t, client, "http://assets.example.com/assets/data.txt"); got != "hello" {
		t.Fatalf("prefix mapping: got %q", got)
	}
}

func TestStaticHandler_PathTraversal(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("home"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := newStaticHandler(root, "/", nil)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.URL.Path = "/../secret.txt"
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("path traversal: want 404, got %d body=%q", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "outside-secret") {
		t.Fatalf("path traversal: leaked outside file content: %q", recorder.Body.String())
	}
}

func TestStaticHandler_ResponseHeaders(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("home"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := newStaticHandler(root, "/", map[string]string{"Access-Control-Allow-Origin": "*"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("response header not applied: got %q", got)
	}
}

func TestProxy_StaticAndRemoteCoexist(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("static-js"), 0o644); err != nil {
		t.Fatal(err)
	}
	up := recordingServer(t, "remote")
	content := "servers:\n" +
		"  - domain: mixed.example.com\n" +
		"    routes:\n" +
		"      - prefix: /assets/\n" +
		"        upstream: " + root + "\n" +
		"      - prefix: /\n" +
		"        upstream: " + up.URL + "\n"
	cfg := writeConfigFile(t, "r.yaml", content)
	proxyURL, _ := startProxy(t, cfg, nil)
	client := proxyClient(proxyURL)

	if got := requestBody(t, client, "http://mixed.example.com/assets/app.js"); got != "static-js" {
		t.Fatalf("static asset: got %q", got)
	}
	if got := requestBody(t, client, "http://mixed.example.com/api/data"); !strings.HasPrefix(got, "remote:/api/data") {
		t.Fatalf("remote route: got %q", got)
	}
}

func TestProxy_ResponseHeadersOverride(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Access-Control-Allow-Origin", "https://restrictive.example.com")
		writer.Header().Set("X-Upstream", "keep")
		io.WriteString(writer, "ok")
	}))
	t.Cleanup(up.Close)
	cfg := writeConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+up.URL+"\n        headers:\n          response:\n            Access-Control-Allow-Origin: \"*\"\n            X-Injected: mrp\n")
	proxyURL, _ := startProxy(t, cfg, nil)
	resp, err := proxyClient(proxyURL).Get("http://api.example.com/data")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("CORS override: got %q", got)
	}
	if got := resp.Header.Get("X-Upstream"); got != "keep" {
		t.Fatalf("unrelated response header should pass: got %q", got)
	}
	if got := resp.Header.Get("X-Injected"); got != "mrp" {
		t.Fatalf("injected header: got %q", got)
	}
}

func TestProxy_RequestHeadersInjected(t *testing.T) {
	var seen http.Header
	up := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen = request.Header.Clone()
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)
	cfg := writeConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+up.URL+"\n        headers:\n          request:\n            Authorization: \"Bearer secret\"\n            X-Trace: mrp\n")
	proxyURL, _ := startProxy(t, cfg, nil)
	resp, err := proxyClient(proxyURL).Get("http://api.example.com/data")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seen.Get("Authorization") != "Bearer secret" {
		t.Fatalf("request Authorization: got %q", seen.Get("Authorization"))
	}
	if seen.Get("X-Trace") != "mrp" {
		t.Fatalf("request X-Trace: got %q", seen.Get("X-Trace"))
	}
}

func TestProxy_HTTPSViaConnect(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, "TLS:"+request.URL.Path)
	}))
	t.Cleanup(up.Close)
	cert := selfSignedCert(t, []string{"api.example.com"})
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12}
	content := "servers:\n" +
		"  - domain: api.example.com\n" +
		"    routes:\n" +
		"      - prefix: /\n" +
		"        upstream: " + up.URL + "\n"
	cfg := writeConfigFile(t, "r.yaml", content)
	proxyURL, _ := startProxy(t, cfg, tlsConfig)
	client := proxyClient(proxyURL)

	got := requestBody(t, client, "https://api.example.com/v1/data")
	if got != "TLS:/v1/data" {
		t.Fatalf("https via connect: got %q", got)
	}
}

func TestServe_DirectTLS(t *testing.T) {
	up := recordingServer(t, "DIRECT")
	cert := selfSignedCert(t, []string{"api.example.com"})
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12}
	content := "servers:\n" +
		"  - domain: api.example.com\n" +
		"    routes:\n" +
		"      - prefix: /\n" +
		"        upstream: " + up.URL + "\n"
	cfg := writeConfigFile(t, "r.yaml", content)
	proxyURL, _ := startProxy(t, cfg, tlsConfig)
	addr := strings.TrimPrefix(proxyURL, "http://")

	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, ServerName: "api.example.com"})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /v1/data HTTP/1.1\r\nHost: api.example.com\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(conn)
	if !strings.Contains(string(body), "DIRECT:/v1/data") {
		t.Fatalf("direct tls: got %q", body)
	}
}

func TestConnect_TunnelBadGateway(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {}))
	closed.Close()
	cfg := writeConfigFile(t, "r.yaml", "servers: []\n")
	proxyURL, _ := startProxy(t, cfg, nil)

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	requestLine := "CONNECT " + strings.TrimPrefix(closed.URL, "http://") + " HTTP/1.1\r\nHost: " + strings.TrimPrefix(closed.URL, "http://") + "\r\n\r\n"
	if _, err := conn.Write([]byte(requestLine)); err != nil {
		t.Fatal(err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want first response 502, got %d", resp.StatusCode)
	}
}

func TestProxy_502OnUnreachable(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {}))
	closed.Close()
	content := "servers:\n" +
		"  - domain: api.example.com\n" +
		"    routes:\n" +
		"      - prefix: /\n" +
		"        upstream: " + closed.URL + "\n"
	cfg := writeConfigFile(t, "r.yaml", content)
	proxyURL, _ := startProxy(t, cfg, nil)
	client := proxyClient(proxyURL)

	resp, err := client.Get("http://api.example.com/x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
}
func TestServe_DirectTLS_RoutesBySNI(t *testing.T) {
	up := recordingServer(t, "SNI")
	caCert, caKey := testAuthorityCA(t)
	authority := newCertificateAuthority(caCert, caKey)
	tlsConfig := &tls.Config{
		GetCertificate: authority.getCertificate,
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}
	content := "servers:\n" +
		"  - domain: api.example.com\n" +
		"    routes:\n" +
		"      - prefix: /\n" +
		"        upstream: " + up.URL + "\n"
	cfg := writeConfigFile(t, "r.yaml", content)
	proxyURL, _ := startProxy(t, cfg, tlsConfig)
	addr := strings.TrimPrefix(proxyURL, "http://")

	// SNI 指向 api.example.com，但 Host 头故意写成 other.com，
	// 验证 TLS 路由用 SNI 而非 Host 头
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, ServerName: "api.example.com"})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /v1/data HTTP/1.1\r\nHost: other.com\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(conn)
	if !strings.Contains(string(body), "SNI:/v1/data") {
		t.Fatalf("应按 SNI 路由命中 api.example.com，got %q", body)
	}
}

func TestReload_SwitchesRoute(t *testing.T) {
	upA := recordingServer(t, "A")
	upB := recordingServer(t, "B")
	cfg := writeConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upA.URL+"\n")
	proxyURL, p := startProxy(t, cfg, nil)
	client := proxyClient(proxyURL)

	if got := requestBody(t, client, "http://api.example.com/x"); !strings.HasPrefix(got, "A:/x") {
		t.Fatalf("before reload: %q", got)
	}
	_ = os.WriteFile(cfg, []byte(
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upB.URL+"\n"), 0o644)
	if err := p.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := requestBody(t, client, "http://api.example.com/x"); !strings.HasPrefix(got, "B:/x") {
		t.Fatalf("after reload: %q", got)
	}
}

func TestEnsureConfig_CreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := ensureConfig(path); err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "servers:") {
		t.Fatalf("默认配置应包含 servers 字段，got %q", data)
	}
	if err := os.WriteFile(path, []byte("custom: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureConfig(path); err != nil {
		t.Fatalf("ensureConfig existing: %v", err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "custom: true\n" {
		t.Fatalf("已存在的文件被覆盖：%q", data)
	}
}

func TestResolveTLSConfig_Defaults(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	cfg, _, err := resolveTLSConfig(certPath, keyPath, false)
	if err != nil || cfg != nil {
		t.Fatalf("want nil config without certs, got %v err=%v", cfg, err)
	}
	if err := os.WriteFile(certPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveTLSConfig(certPath, keyPath, false); err == nil {
		t.Fatal("want error for missing key")
	}
}

func testAuthorityCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return caCert, key
}

func TestCertificateAuthority_SignsForSNI(t *testing.T) {
	caCert, caKey := testAuthorityCA(t)
	authority := newCertificateAuthority(caCert, caKey)

	hello := &tls.ClientHelloInfo{ServerName: "api.example.com"}
	cert, err := authority.getCertificate(hello)
	if err != nil {
		t.Fatalf("getCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "api.example.com" {
		t.Fatalf("SAN 应为 api.example.com，got %v", leaf.DNSNames)
	}
	if err := leaf.CheckSignatureFrom(caCert); err != nil {
		t.Fatalf("证书应由 CA 签发：%v", err)
	}

	cached, err := authority.getCertificate(hello)
	if err != nil {
		t.Fatal(err)
	}
	if cached != cert {
		t.Fatal("同一 SNI 应命中缓存返回同一证书")
	}

	// 混合大小写 SNI 应归一化为小写，复用缓存
	mixed, err := authority.getCertificate(&tls.ClientHelloInfo{ServerName: "API.Example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if mixed != cert {
		t.Fatal("混合大小写 SNI 应复用同一缓存证书")
	}

	if _, err := authority.getCertificate(&tls.ClientHelloInfo{ServerName: ""}); err == nil {
		t.Fatal("缺少 SNI 应报错")
	}
}

func TestCertificateAuthority_ClearCache(t *testing.T) {
	caCert, caKey := testAuthorityCA(t)
	authority := newCertificateAuthority(caCert, caKey)
	hello := &tls.ClientHelloInfo{ServerName: "api.example.com"}

	cert, err := authority.getCertificate(hello)
	if err != nil {
		t.Fatal(err)
	}
	if cached, _ := authority.getCertificate(hello); cached != cert {
		t.Fatal("同一 SNI 应命中缓存返回同一证书")
	}
	authority.clearCache()
	renewed, err := authority.getCertificate(hello)
	if err != nil {
		t.Fatal(err)
	}
	if renewed == cert {
		t.Fatal("clearCache 后应重新签发证书，而非复用旧缓存")
	}
}

func TestCertificateAuthority_ConcurrentSameSNI(t *testing.T) {
	caCert, caKey := testAuthorityCA(t)
	authority := newCertificateAuthority(caCert, caKey)
	hello := &tls.ClientHelloInfo{ServerName: "api.example.com"}

	const n = 8
	results := make([]*tls.Certificate, n)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := 0; i < n; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			cert, err := authority.getCertificate(hello)
			if err != nil {
				t.Errorf("getCertificate: %v", err)
				return
			}
			results[i] = cert
		}(i)
	}
	start.Done()
	done.Wait()
	for i := 1; i < n; i++ {
		if results[i] != results[0] {
			t.Fatal("同域名并发握手应合并为一次签发，复用同一证书")
		}
	}
}

func TestCertificateAuthority_ConcurrentDistinctSNI(t *testing.T) {
	caCert, caKey := testAuthorityCA(t)
	authority := newCertificateAuthority(caCert, caKey)

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hello := &tls.ClientHelloInfo{ServerName: fmt.Sprintf("host%d.example.com", i)}
			cert, err := authority.getCertificate(hello)
			if err != nil || cert == nil {
				t.Errorf("sign host%d: cert=%v err=%v", i, cert, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestWatch_HotReload(t *testing.T) {
	upA := recordingServer(t, "A")
	upB := recordingServer(t, "B")
	cfg := writeConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upA.URL+"\n")
	proxyURL, p := startProxy(t, cfg, nil)
	client := proxyClient(proxyURL)

	stop := make(chan struct{})
	go p.watchFile(20*time.Millisecond, stop)
	t.Cleanup(func() { close(stop) })

	if got := requestBody(t, client, "http://api.example.com/x"); !strings.HasPrefix(got, "A:/x") {
		t.Fatalf("before hot reload: %q", got)
	}
	_ = os.WriteFile(cfg, []byte(
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upB.URL+"\n"), 0o644)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if got := requestBody(t, client, "http://api.example.com/x"); strings.HasPrefix(got, "B:/x") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("配置变更未自动热加载")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func echoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { io.Copy(c, c); c.Close() }(conn)
		}
	}()
	return listener.Addr().String()
}

func readConnectResponse(t *testing.T, reader *bufio.Reader) int {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	var code int
	if _, err := fmt.Sscanf(line, "HTTP/1.%d %d", new(int), &code); err != nil {
		t.Fatalf("parse status %q: %v", line, err)
	}
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	return code
}

// 直连 TLS 后内部发 CONNECT：验证 hijack 后 handleConn 不误关连接
func TestServe_DirectTLS_NestedConnectTunnel(t *testing.T) {
	echo := echoServer(t)
	caCert, caKey := testAuthorityCA(t)
	tlsConfig := &tls.Config{
		GetCertificate: newCertificateAuthority(caCert, caKey).getCertificate,
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}
	cfg := writeConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: http://unused\n")
	proxyURL, _ := startProxy(t, cfg, tlsConfig)
	addr := strings.TrimPrefix(proxyURL, "http://")

	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, ServerName: "api.example.com"})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echo, echo)
	if code := readConnectResponse(t, reader); code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
	if _, err := conn.Write([]byte("PING\n")); err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadString('\n')
	if err != nil || line != "PING\n" {
		t.Fatalf("echo: got %q err=%v", line, err)
	}
}

func TestConnect_PipelinedDataPreserved(t *testing.T) {
	echo := echoServer(t)
	cfg := writeConfigFile(t, "r.yaml", "servers: []\n")
	proxyURL, _ := startProxy(t, cfg, nil)
	addr := strings.TrimPrefix(proxyURL, "http://")

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// CONNECT 和数据写在同一次 Write 中，模拟客户端紧跟 CONNECT 发送数据
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nPING\n", echo, echo)
	reader := bufio.NewReader(conn)
	if code := readConnectResponse(t, reader); code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
	line, err := reader.ReadString('\n')
	if err != nil || line != "PING\n" {
		t.Fatalf("pipelined data should be forwarded: got %q err=%v", line, err)
	}
}

// CONNECT+MITM 后内部再发 CONNECT：验证嵌套 hijack 不被外层 defer 误关
func TestConnect_NestedConnectViaMITM(t *testing.T) {
	echo := echoServer(t)
	caCert, caKey := testAuthorityCA(t)
	tlsConfig := &tls.Config{
		GetCertificate: newCertificateAuthority(caCert, caKey).getCertificate,
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}
	cfg := writeConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: http://unused\n")
	proxyURL, _ := startProxy(t, cfg, tlsConfig)
	addr := strings.TrimPrefix(proxyURL, "http://")

	proxyConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer proxyConn.Close()
	fmt.Fprintf(proxyConn, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n")
	outerReader := bufio.NewReader(proxyConn)
	if code := readConnectResponse(t, outerReader); code != 200 {
		t.Fatalf("outer CONNECT: want 200, got %d", code)
	}
	innerConn := tls.Client(proxyConn, &tls.Config{InsecureSkipVerify: true, ServerName: "api.example.com"})
	if err := innerConn.HandshakeContext(t.Context()); err != nil {
		t.Fatalf("inner handshake: %v", err)
	}
	reader := bufio.NewReader(innerConn)
	fmt.Fprintf(innerConn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echo, echo)
	if code := readConnectResponse(t, reader); code != 200 {
		t.Fatalf("nested CONNECT: want 200, got %d", code)
	}
	if _, err := innerConn.Write([]byte("PING\n")); err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadString('\n')
	if err != nil || line != "PING\n" {
		t.Fatalf("echo: got %q err=%v", line, err)
	}
}

var testStamp = time.Date(2026, 9, 28, 9, 21, 36, 865_000_000, time.FixedZone("CST", 8*3600))

func TestFormatLogLine(t *testing.T) {
	got := formatLogLine(testStamp, logInfo, "created default config file path=config.yaml", false)
	want := "09-28 09:21:36.865\tINFO\tcreated default config file path=config.yaml\n"
	if got != want {
		t.Fatalf("format mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestFormatLogLine_Color(t *testing.T) {
	for _, tc := range []struct {
		level logLevel
		code  string
		reset bool
	}{
		{logDebug, "\x1b[90m", true},
		{logInfo, "", false},
		{logWarn, "\x1b[33m", true},
		{logError, "\x1b[31m", true},
	} {
		got := formatLogLine(testStamp, tc.level, "event", true)
		want := tc.code + "09-28 09:21:36.865\t" + tc.level.String() + "\tevent"
		if tc.reset {
			want += "\x1b[0m"
		}
		want += "\n"
		if got != want {
			t.Fatalf("level %v color mismatch:\n got %q\nwant %q", tc.level, got, want)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	for text, want := range map[string]logLevel{
		"debug": logDebug,
		"info":  logInfo,
		"warn":  logWarn,
		"error": logError,
	} {
		got, err := parseLogLevel(text)
		if err != nil || got != want {
			t.Fatalf("parseLogLevel(%q) = %v, %v", text, got, err)
		}
	}
	if _, err := parseLogLevel("verbose"); err == nil {
		t.Fatal("unknown level should fail")
	}
}

func TestLogf_LevelFilter(t *testing.T) {
	var out bytes.Buffer
	logOut = &out
	logFloor = logWarn
	t.Cleanup(func() { logOut = io.Discard; logFloor = logInfo })

	logInfof("hidden")
	logWarnf("shown")
	if out.String() != formatLogLine(time.Now(), logWarn, "shown", false) {
		t.Fatalf("filter mismatch: %q", out.String())
	}
}

func TestNormalizeNameserver(t *testing.T) {
	tests := []struct {
		input string
		want  string
		fail  bool
	}{
		{"114.114.114.114", "114.114.114.114:53", false},
		{"8.8.8.8:5353", "8.8.8.8:5353", false},
		{"[::1]:53", "[::1]:53", false},
		{"  1.1.1.1  ", "1.1.1.1:53", false},
		{"", "", true},
		{"dns.example.com", "", true},
		{"127.0.0.1:abc", "", true},
		{"127.0.0.1:0", "", true},
		{"127.0.0.1:65536", "", true},
	}
	for _, tc := range tests {
		got, err := normalizeNameserver(tc.input)
		if tc.fail {
			if err == nil {
				t.Fatalf("normalizeNameserver(%q) accepted %q", tc.input, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("normalizeNameserver(%q): %v", tc.input, err)
		}
		if got != tc.want {
			t.Fatalf("normalizeNameserver(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestNormalizeNameservers_Defaults(t *testing.T) {
	addresses, err := normalizeNameservers(nil)
	if err != nil {
		t.Fatalf("normalizeNameservers: %v", err)
	}
	want := []string{"114.114.114.114:53", "8.8.8.8:53"}
	if !slices.Equal(addresses, want) {
		t.Fatalf("defaults = %v, want %v", addresses, want)
	}
}

func TestDefaultConfig_Parses(t *testing.T) {
	table, nameservers, err := loadTable(writeConfigFile(t, "default.yaml", defaultConfig))
	if err != nil {
		t.Fatalf("loadTable(defaultConfig): %v", err)
	}
	if len(table.byDomain) != 0 {
		t.Fatalf("default table = %v", table)
	}
	if len(nameservers) != 0 {
		t.Fatalf("default nameservers = %v", nameservers)
	}
}

func TestNameserverSet_Failover(t *testing.T) {
	echo := echoServer(t)
	_, port, _ := net.SplitHostPort(echo)
	stub := startDNSStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	servers := testNameservers(t)
	if err := servers.update([]string{"127.0.0.1:" + closedUDPPort(t), stub.address()}); err != nil {
		t.Fatalf("update: %v", err)
	}
	conn, err := servers.DialContext(context.Background(), "tcp", "echo.mrp.local:"+port)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	payload := []byte("ping")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(echoed, payload) {
		t.Fatalf("echo = %q, want %q", echoed, payload)
	}
}

func TestNameserverSet_UpdateFailureKeepsServers(t *testing.T) {
	stub := startDNSStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	servers := testNameservers(t, stub.address())
	if err := servers.update([]string{"dns.example.com"}); err == nil {
		t.Fatal("hostname nameserver should be rejected")
	}
	if got := servers.serverAddresses(); !slices.Equal(got, []string{stub.address()}) {
		t.Fatalf("nameservers after failed update = %v, want %v", got, []string{stub.address()})
	}
}

func TestProxy_UpstreamResolvedByNameserver(t *testing.T) {
	up := recordingServer(t, "upstream")
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	stub := startDNSStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	config := "servers:\n" +
		"  - domain: api.example.com\n" +
		"    routes:\n" +
		"      - prefix: /\n" +
		"        upstream: http://api.mrp.local:" + port + "\n" +
		"nameservers:\n" +
		"  - \"127.0.0.1:" + closedUDPPort(t) + "\"\n" +
		"  - \"" + stub.address() + "\"\n"
	proxyURL, _ := startProxy(t, writeConfigFile(t, "r.yaml", config), nil)
	if got := requestBody(t, proxyClient(proxyURL), "http://api.example.com/hello"); !strings.HasPrefix(got, "upstream:/hello") {
		t.Fatalf("nameserver routing: got %q", got)
	}
}

func TestProxy_ReloadNameservers(t *testing.T) {
	stub := startDNSStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	refused := "127.0.0.1:" + closedUDPPort(t)
	config := "servers:\n" +
		"  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: http://unused\n" +
		"nameservers:\n  - \"" + refused + "\"\n"
	servers := testNameservers(t)
	path := writeConfigFile(t, "r.yaml", config)
	p, err := newProxy(path, newTransport(servers.DialContext), nil, nil, servers)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	if got := servers.serverAddresses(); !slices.Equal(got, []string{refused}) {
		t.Fatalf("initial nameservers = %v, want %v", got, []string{refused})
	}
	if err := os.WriteFile(path, []byte(strings.Replace(config, refused, stub.address(), 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := servers.serverAddresses(); !slices.Equal(got, []string{stub.address()}) {
		t.Fatalf("reloaded nameservers = %v, want %v", got, []string{stub.address()})
	}
}

func closedUDPPort(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()
	return strconv.Itoa(port)
}

type dnsStub struct {
	listener *net.UDPConn
	records  map[uint16][]net.IP
}

func startDNSStub(t *testing.T, records map[uint16][]net.IP) *dnsStub {
	t.Helper()
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	stub := &dnsStub{listener: listener.(*net.UDPConn), records: records}
	t.Cleanup(func() { _ = stub.listener.Close() })
	go stub.serve()
	return stub
}

func (s *dnsStub) address() string {
	return s.listener.LocalAddr().String()
}

func (s *dnsStub) serve() {
	buf := make([]byte, 1500)
	for {
		n, remote, err := s.listener.ReadFrom(buf)
		if err != nil {
			return
		}
		id, name, qtype, ok := parseDNSQuestion(buf[:n])
		if !ok {
			continue
		}
		_, _ = s.listener.WriteTo(dnsReply(id, name, qtype, s.records[qtype]), remote)
	}
}

func parseDNSQuestion(data []byte) (uint16, string, uint16, bool) {
	if len(data) < 12 {
		return 0, "", 0, false
	}
	id := binary.BigEndian.Uint16(data[0:2])
	if binary.BigEndian.Uint16(data[4:6]) != 1 {
		return 0, "", 0, false
	}
	offset := 12
	name := ""
	for {
		if offset >= len(data) {
			return 0, "", 0, false
		}
		length := int(data[offset])
		if length == 0 {
			offset++
			break
		}
		if length&0xc0 != 0 || offset+1+length > len(data) {
			return 0, "", 0, false
		}
		name += "." + string(data[offset+1:offset+1+length])
		offset += 1 + length
	}
	if offset+4 > len(data) {
		return 0, "", 0, false
	}
	return id, strings.TrimPrefix(name, "."), binary.BigEndian.Uint16(data[offset : offset+2]), true
}

func dnsReply(id uint16, name string, qtype uint16, records []net.IP) []byte {
	header := make([]byte, 12)
	binary.BigEndian.PutUint16(header[0:2], id)
	binary.BigEndian.PutUint16(header[2:4], 0x8180)
	binary.BigEndian.PutUint16(header[4:6], 1)
	binary.BigEndian.PutUint16(header[6:8], uint16(len(records)))
	binary.BigEndian.PutUint16(header[8:10], 0)
	binary.BigEndian.PutUint16(header[10:12], 0)
	reply := append(header, dnsQuestion(name, qtype)...)
	for _, record := range records {
		reply = append(reply, dnsAnswer(qtype, record)...)
	}
	return reply
}

func dnsQuestion(name string, qtype uint16) []byte {
	var question []byte
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 {
			continue
		}
		question = append(question, byte(len(label)))
		question = append(question, label...)
	}
	question = append(question, 0)
	tail := make([]byte, 4)
	binary.BigEndian.PutUint16(tail[0:2], qtype)
	binary.BigEndian.PutUint16(tail[2:4], 1)
	return append(question, tail...)
}

func dnsAnswer(qtype uint16, address net.IP) []byte {
	answer := make([]byte, 12)
	binary.BigEndian.PutUint16(answer[0:2], 0xc00c)
	binary.BigEndian.PutUint16(answer[2:4], qtype)
	binary.BigEndian.PutUint16(answer[4:6], 1)
	rdata := address.To16()
	if v4 := address.To4(); v4 != nil {
		rdata = v4
	}
	binary.BigEndian.PutUint16(answer[10:12], uint16(len(rdata)))
	return append(answer, rdata...)
}
