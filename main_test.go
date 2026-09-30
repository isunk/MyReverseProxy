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
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// withProxyPort 在每条 server 条目的 domain 行后插入 port 字段，
// 供测试把随机可绑定端口写进配置，让 listenerSet.reconcile 能在测试环境监听。
func withProxyPort(content string, port int) string {
	var builder strings.Builder
	for _, line := range strings.SplitAfter(content, "\n") {
		builder.WriteString(line)
		if strings.HasPrefix(line, "  - domain:") {
			fmt.Fprintf(&builder, "    port: %d\n", port)
		}
	}
	return builder.String()
}

// testPort 分配一个当前空闲的 TCP 端口供测试配置使用
func testPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// startProxyWithNameservers 注入随机端口到配置后构建代理：newProxy 的 reload 会
// 经 listenerSet.reconcile 监听该端口，调用方拿到可直接访问的代理地址。
func startProxyWithNameservers(t *testing.T, configPath string, tlsConfig *tls.Config, servers *nameserverSet) (string, *proxy) {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	port := testPort(t)
	if err := os.WriteFile(configPath, []byte(withProxyPort(string(data), port)), 0o644); err != nil {
		t.Fatal(err)
	}
	transport := newTransport(servers.DialContext)
	p, err := newProxy(configPath, transport, tlsConfig, nil, servers)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	t.Cleanup(func() { p.listeners.closeAll() })
	return fmt.Sprintf("http://127.0.0.1:%d", port), p
}

func testNameservers(t *testing.T, entries ...string) *nameserverSet {
	t.Helper()
	return testNameserversWithTimeout(t, defaultAttemptTimeout, defaultDNSTTL, entries)
}

// testNameserversWithTimeout 显式指定单次尝试超时与 DNS 缓存 TTL，TTL 为 0 时禁用缓存
func testNameserversWithTimeout(t *testing.T, attemptTimeout, dnsTTL time.Duration, entries []string) *nameserverSet {
	t.Helper()
	servers, err := newNameserverSet(attemptTimeout, dnsTTL, entries)
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

// getOnce 请求一次并立即关闭响应体，使上游连接回到空闲池以便观测连接复用。
func getOnce(t *testing.T, client *http.Client, rawURL, prefix string) {
	t.Helper()
	resp, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(body)), prefix) {
		t.Fatalf("GET %s = %q, want prefix %q", rawURL, body, prefix)
	}
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
	if len(table.byPort) != 1 {
		t.Fatalf("want 1 port group, got %d", len(table.byPort))
	}
	entry, ok := table.pick(80, "a.example.com", "/v1/x")
	if !ok || entry.prefix != "/v1/" {
		t.Fatalf("pick /v1/: got %+v ok=%v", entry, ok)
	}
	if _, ok := table.pick(80, "b.example.com", "/"); !ok {
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
			entry, ok := table.pick(80, "a.example.com", "/")
			if !ok {
				t.Fatalf("route not found for upstream %q", up)
			}
			if entry.target.root != up {
				t.Fatalf("target.root: got %q want %q", entry.target.root, up)
			}
			if entry.target.url != nil {
				t.Fatalf("target.url should be nil for local path %q", up)
			}
		})
	}
}

func TestPick_LongestPrefix(t *testing.T) {
	table := &routeTable{byPort: map[int]*portGroup{
		80: {protocol: protocolHTTP, byDomain: map[string][]*route{
			"a": {
				{prefix: "/"},
				{prefix: "/v1/"},
				{prefix: "/v1/users/"},
				{prefix: "/api"},
			},
		}},
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
		entry, ok := table.pick(80, "a", path)
		if !ok || entry.prefix != want {
			t.Fatalf("pick %s: want %s got %+v ok=%v", path, want, entry, ok)
		}
	}
	if _, ok := table.pick(80, "other", "/"); ok {
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
	if !table.has(80, "api.example.com") {
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
	if !table.has(80, "api.example.com") {
		t.Fatal("domain should be trimmed")
	}
	entry, ok := table.pick(80, "api.example.com", "/v1/x")
	if !ok || entry.prefix != "/v1/" {
		t.Fatalf("prefix should be trimmed: got %+v ok=%v", entry, ok)
	}
	if entry.host != "up-a.example.com" {
		t.Fatalf("host should be trimmed: got %q", entry.host)
	}
}

func TestRouteTable_Fingerprint(t *testing.T) {
	base := "servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: http://up-a:8080\n"
	loaded := func(content string) *routeTable {
		table, _, err := loadTable(writeConfigFile(t, "r.yaml", content))
		if err != nil {
			t.Fatalf("loadTable: %v", err)
		}
		return table
	}
	same := func(a, b *routeTable, what string) {
		if a.fingerprint() != b.fingerprint() {
			t.Fatalf("%s must not change the fingerprint: %q != %q", what, a.fingerprint(), b.fingerprint())
		}
	}
	different := func(a, b *routeTable, what string) {
		if a.fingerprint() == b.fingerprint() {
			t.Fatalf("%s must change the fingerprint", what)
		}
	}
	headerOnly := strings.Replace(base, "upstream: http://up-a:8080\n",
		"upstream: http://up-a:8080\n        headers:\n          response:\n            X-Test: \"1\"\n", 1)
	same(loaded(base), loaded(base), "identical config")
	different(loaded(base), loaded(strings.Replace(base, "up-a", "up-b", 1)), "upstream change")
	different(loaded(base), loaded(headerOnly), "header-only change")
	same(loaded(base), loaded(base+"nameservers:\n  - \"127.0.0.1:53\"\n"), "nameservers-only change")
}

func TestRouteTable_InstallHandlers(t *testing.T) {
	table, _, err := loadTable(writeConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n"+
			"      - prefix: /\n        upstream: http://up-a:8080\n"+
			"      - prefix: /api/\n        upstream: http://up-b:8080\n"+
			"        headers:\n          response:\n            X-Test: \"1\"\n"+
			"      - prefix: /files/\n        upstream: ./dist\n"))
	if err != nil {
		t.Fatalf("loadTable: %v", err)
	}
	table.installHandlers(newTransport(nil))

	remote, ok := table.pick(80, "api.example.com", "/v1")
	if !ok || remote.handler == nil {
		t.Fatalf("remote route must get a handler: %+v ok=%v", remote, ok)
	}
	if handler, ok := remote.handler.(*httputil.ReverseProxy); !ok {
		t.Fatalf("remote route handler: got %T", remote.handler)
	} else if handler.ModifyResponse != nil {
		t.Fatal("route without response headers must not mount a modify hook")
	}
	hooked, ok := table.pick(80, "api.example.com", "/api/x")
	if !ok {
		t.Fatal("/api/ prefix should match")
	}
	if handler, ok := hooked.handler.(*httputil.ReverseProxy); !ok || handler.ModifyResponse == nil {
		t.Fatalf("route with response headers must mount a modify hook: %T", hooked.handler)
	}
	files, ok := table.pick(80, "api.example.com", "/files/a")
	if !ok {
		t.Fatal("/files/ prefix should match")
	}
	if _, ok := files.handler.(*staticHandler); !ok {
		t.Fatalf("local route handler: got %T", files.handler)
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
	if _, _, err := loadTLSConfig(certPath, keyPath, true); err == nil {
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
		"    protocol: https\n" +
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
	cfg := writeConfigFile(t, "r.yaml",
		"servers:\n  - domain: placeholder.example.com\n    routes:\n      - prefix: /\n        upstream: http://127.0.0.1:1\n")
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
		"    protocol: https\n" +
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

// protocol 省略时端口自适应：同一端口按连接首字节分别服务 HTTP 明文与 HTTPS 直连
func TestServe_AutoProtocolAdapts(t *testing.T) {
	up := recordingServer(t, "AUTO")
	caCert, caKey := testAuthorityCA(t)
	tlsConfig := &tls.Config{
		GetCertificate: newCertificateAuthority(caCert, caKey).getCertificate,
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}
	cfg := writeConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+up.URL+"\n")
	proxyURL, _ := startProxy(t, cfg, tlsConfig)

	if got := requestBody(t, proxyClient(proxyURL), "http://api.example.com/x"); !strings.HasPrefix(got, "AUTO:/x") {
		t.Fatalf("http: got %q", got)
	}

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
	if !strings.Contains(string(body), "AUTO:/v1/data") {
		t.Fatalf("https: got %q", body)
	}
}

func TestReload_SwitchesRoute(t *testing.T) {
	upA := recordingServer(t, "A")
	upB := recordingServer(t, "B")
	port := testPort(t)
	cfg := writeConfigFile(t, "r.yaml", withProxyPort(
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upA.URL+"\n", port))
	servers := testNameservers(t)
	p, err := newProxy(cfg, newTransport(servers.DialContext), nil, nil, servers)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	t.Cleanup(func() { p.listeners.closeAll() })
	client := proxyClient(fmt.Sprintf("http://127.0.0.1:%d", port))

	if got := requestBody(t, client, "http://api.example.com/x"); !strings.HasPrefix(got, "A:/x") {
		t.Fatalf("before reload: %q", got)
	}
	_ = os.WriteFile(cfg, []byte(withProxyPort(
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upB.URL+"\n", port)), 0o644)
	if err := p.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := requestBody(t, client, "http://api.example.com/x"); !strings.HasPrefix(got, "B:/x") {
		t.Fatalf("after reload: %q", got)
	}
}

func TestReload_UnchangedConfigKeepsIdleConnections(t *testing.T) {
	upA := recordingServer(t, "A")
	upB := recordingServer(t, "B")
	config := "servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: " + upA.URL + "\n"
	port := testPort(t)
	path := writeConfigFile(t, "r.yaml", withProxyPort(config, port))

	var dials int32
	servers := testNameservers(t)
	dialing := func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := servers.DialContext(ctx, network, address)
		if err == nil {
			atomic.AddInt32(&dials, 1)
		}
		return conn, err
	}
	p, err := newProxy(path, newTransport(dialing), nil, nil, servers)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	t.Cleanup(func() { p.listeners.closeAll() })
	client := proxyClient(fmt.Sprintf("http://127.0.0.1:%d", port))

	getOnce(t, client, "http://api.example.com/one", "A:/one")
	if got := atomic.LoadInt32(&dials); got != 1 {
		t.Fatalf("dials after first request = %d, want 1", got)
	}
	if err := p.reload(); err != nil {
		t.Fatalf("reload unchanged config: %v", err)
	}
	getOnce(t, client, "http://api.example.com/two", "A:/two")
	if got := atomic.LoadInt32(&dials); got != 1 {
		t.Fatalf("dials after unchanged reload = %d, want 1 (idle upstream connection was torn down)", got)
	}
	// 真正变更仍需重建路由并拆掉指向旧目标的连接
	if err := os.WriteFile(path, []byte(withProxyPort(strings.Replace(config, upA.URL, upB.URL, 1), port)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.reload(); err != nil {
		t.Fatalf("reload changed config: %v", err)
	}
	getOnce(t, client, "http://api.example.com/three", "B:/three")
	if got := atomic.LoadInt32(&dials); got != 2 {
		t.Fatalf("dials after route change = %d, want 2", got)
	}
}

// 路由变更后清掉解析缓存；仅改注释不触发
func TestReload_ClearsDNSCache(t *testing.T) {
	up := recordingServer(t, "upstream")
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	stub := startDNSStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	config := "servers:\n" +
		"  - domain: api.example.com\n" +
		"    routes:\n" +
		"      - prefix: /\n" +
		"        upstream: http://api.mrp.local:" + port + "\n" +
		"nameservers:\n" +
		"  - \"" + stub.address() + "\"\n"
	proxyPort := testPort(t)
	servers := testNameservers(t)
	path := writeConfigFile(t, "r.yaml", withProxyPort(config, proxyPort))
	p, err := newProxy(path, newTransport(servers.DialContext), nil, nil, servers)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	t.Cleanup(func() { p.listeners.closeAll() })
	client := proxyClient(fmt.Sprintf("http://127.0.0.1:%d", proxyPort))

	getOnce(t, client, "http://api.example.com/one", "upstream:/one")
	if got := servers.cache.size(); got != 1 {
		t.Fatalf("cached entries after first request = %d, want 1", got)
	}
	if got := stub.queryCount(); got != 2 {
		t.Fatalf("queries after first request = %d, want 2", got)
	}
	// 仅改注释：不重建路由，也不清解析缓存
	if err := os.WriteFile(path, []byte(withProxyPort("# comment only\n"+config, proxyPort)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.reload(); err != nil {
		t.Fatalf("reload comment-only change: %v", err)
	}
	if got := servers.cache.size(); got != 1 {
		t.Fatalf("cached entries after comment-only reload = %d, want 1", got)
	}
	// 改上游地址：必须丢弃旧解析结果
	if err := os.WriteFile(path, []byte(withProxyPort(strings.Replace(config, "api.mrp.local", "api2.mrp.local", 1), proxyPort)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.reload(); err != nil {
		t.Fatalf("reload upstream change: %v", err)
	}
	if got := servers.cache.size(); got != 0 {
		t.Fatalf("cached entries after upstream change = %d, want 0", got)
	}
	getOnce(t, client, "http://api.example.com/two", "upstream:/two")
	if got := stub.queryCount(); got != 4 {
		t.Fatalf("queries after upstream change = %d, want 4", got)
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

func TestLoadTLSConfig_Defaults(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	cfg, _, err := loadTLSConfig(certPath, keyPath, false)
	if err != nil || cfg != nil {
		t.Fatalf("want nil config without certs, got %v err=%v", cfg, err)
	}
	if err := os.WriteFile(certPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadTLSConfig(certPath, keyPath, false); err == nil {
		t.Fatal("want error for missing key")
	}
}

func TestTransport_CachesTLSSessions(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, "ok")
	}))
	defer upstream.Close()
	transport := newTransport(func(ctx context.Context, network, address string) (net.Conn, error) {
		return net.Dial(network, address)
	})
	transport.MaxIdleConns = 0
	transport.MaxIdleConnsPerHost = 0
	transport.DisableKeepAlives = true
	requested := upstream.URL + "/"
	// NewSessionTicket 是 TLS 1.3 握手后消息，连接立即关闭时可能来不及送达，故多次尝试
	var resumed bool
	for i := range 3 {
		httpReq, err := http.NewRequest(http.MethodGet, requested, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := transport.RoundTrip(httpReq)
		if err != nil {
			t.Fatalf("roundtrip %d: %v", i+1, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.TLS == nil {
			t.Fatalf("roundtrip %d: missing tls state", i+1)
		}
		if i == 0 && resp.TLS.DidResume {
			t.Fatal("first upstream connection resumed unexpectedly")
		}
		resumed = resumed || resp.TLS.DidResume
	}
	if !resumed {
		t.Fatal("no upstream connection resumed the tls session")
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
	for i := range n {
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
	for i := range n {
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
	port := testPort(t)
	cfg := writeConfigFile(t, "r.yaml", withProxyPort(
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upA.URL+"\n", port))
	servers := testNameservers(t)
	p, err := newProxy(cfg, newTransport(servers.DialContext), nil, nil, servers)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	t.Cleanup(func() { p.listeners.closeAll() })
	client := proxyClient(fmt.Sprintf("http://127.0.0.1:%d", port))

	stop := make(chan struct{})
	go p.watchFile(20*time.Millisecond, stop)
	t.Cleanup(func() { close(stop) })

	if got := requestBody(t, client, "http://api.example.com/x"); !strings.HasPrefix(got, "A:/x") {
		t.Fatalf("before hot reload: %q", got)
	}
	_ = os.WriteFile(cfg, []byte(withProxyPort(
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upB.URL+"\n", port)), 0o644)

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
		"servers:\n  - domain: api.example.com\n    protocol: https\n    routes:\n      - prefix: /\n        upstream: http://unused\n")
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
	cfg := writeConfigFile(t, "r.yaml",
		"servers:\n  - domain: placeholder.example.com\n    routes:\n      - prefix: /\n        upstream: http://127.0.0.1:1\n")
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
	if len(table.byPort) != 0 {
		t.Fatalf("default table = %v", table)
	}
	if len(nameservers) != 0 {
		t.Fatalf("default nameservers = %v", nameservers)
	}
}

func TestDefaultConfig_DocumentsNameservers(t *testing.T) {
	for _, line := range []string{"# nameservers:", "#   - \"114.114.114.114\"", "nameservers: []"} {
		if !strings.Contains(defaultConfig, line) {
			t.Fatalf("defaultConfig missing %q", line)
		}
	}
}

func TestLoadTable_RejectsUnknownField(t *testing.T) {
	config := "servers: []\nnameserver: [\"114.114.114.114\"]\n"
	if _, _, err := loadTable(writeConfigFile(t, "typo.yaml", config)); err == nil {
		t.Fatal("loadTable accepted unknown field nameserver")
	}
}

func TestLoadTable_AcceptsUTF8BOM(t *testing.T) {
	config := "\ufeffservers: []\nnameservers: [\"114.114.114.114\"]\n"
	if _, _, err := loadTable(writeConfigFile(t, "bom.yaml", config)); err != nil {
		t.Fatalf("loadTable(bom): %v", err)
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

// 首个 nameserver 只收包不回复，验证单次尝试超时会真正生效并限制故障切换等待
func TestNameserverSet_AttemptTimeout(t *testing.T) {
	echo := echoServer(t)
	_, port, _ := net.SplitHostPort(echo)
	stub := startDNSStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	servers := testNameserversWithTimeout(t, 50*time.Millisecond, defaultDNSTTL,
		[]string{"127.0.0.1:" + silentUDPPort(t), stub.address()})
	started := time.Now()
	conn, err := servers.DialContext(context.Background(), "tcp", "echo.mrp.local:"+port)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if elapsed < 40*time.Millisecond {
		t.Fatalf("failover took %v, first nameserver was not waited on", elapsed)
	}
	if elapsed > time.Second {
		t.Fatalf("failover took %v, per-nameserver attempt timeout not applied", elapsed)
	}
}

// 首次连接解析并缓存，同主机:端口的后续连接直接复用解析结果
func TestNameserverSet_CachesResolution(t *testing.T) {
	echo := echoServer(t)
	_, port, _ := net.SplitHostPort(echo)
	stub := startDNSStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	servers := testNameservers(t, stub.address())
	for i := range 3 {
		conn, err := servers.DialContext(context.Background(), "tcp", "up.example.com:"+port)
		if err != nil {
			t.Fatalf("dial %d: %v", i+1, err)
		}
		conn.Close()
	}
	if got := stub.queryCount(); got != 2 {
		t.Fatalf("dns queries for 3 dials = %d, want 2 (one lookup then cache hits)", got)
	}
}

func TestNameserverSet_CacheTTLExpires(t *testing.T) {
	echo := echoServer(t)
	_, port, _ := net.SplitHostPort(echo)
	stub := startDNSStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	servers := testNameserversWithTimeout(t, defaultAttemptTimeout, 20*time.Millisecond, []string{stub.address()})
	dial := func() {
		conn, err := servers.DialContext(context.Background(), "tcp", "up.example.com:"+port)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		conn.Close()
	}
	dial()
	if got := stub.queryCount(); got != 2 {
		t.Fatalf("queries before expiry = %d, want 2", got)
	}
	time.Sleep(50 * time.Millisecond)
	dial()
	if got := stub.queryCount(); got != 4 {
		t.Fatalf("queries after ttl expiry = %d, want 4", got)
	}
}

// TTL 为 0 时禁用缓存，每次连接都重新解析
func TestNameserverSet_CacheDisabled(t *testing.T) {
	echo := echoServer(t)
	_, port, _ := net.SplitHostPort(echo)
	stub := startDNSStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	servers := testNameserversWithTimeout(t, defaultAttemptTimeout, 0, []string{stub.address()})
	for i := range 2 {
		conn, err := servers.DialContext(context.Background(), "tcp", "up.example.com:"+port)
		if err != nil {
			t.Fatalf("dial %d: %v", i+1, err)
		}
		conn.Close()
	}
	if got := stub.queryCount(); got != 4 {
		t.Fatalf("queries with cache disabled = %d, want 4", got)
	}
	if got := servers.cache.size(); got != 0 {
		t.Fatalf("cached entries with cache disabled = %d, want 0", got)
	}
}

// 缓存的地址连不上时立即重解析，不等 TTL 过期
func TestNameserverSet_StaleCacheRetries(t *testing.T) {
	echo := echoServer(t)
	_, port, _ := net.SplitHostPort(echo)
	stub := startDNSStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.2")}})
	servers := testNameservers(t, stub.address())
	if _, err := servers.DialContext(context.Background(), "tcp", "up.example.com:"+port); err == nil {
		t.Fatal("dial to an unreachable address should fail")
	}
	if got := stub.queryCount(); got != 2 {
		t.Fatalf("queries after first dial = %d, want 2", got)
	}
	stub.setRecords(map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	conn, err := servers.DialContext(context.Background(), "tcp", "up.example.com:"+port)
	if err != nil {
		t.Fatalf("dial after upstream moved: %v", err)
	}
	payload := []byte("ping")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = conn.Close()
	if !bytes.Equal(echoed, payload) {
		t.Fatalf("echo = %q, want %q", echoed, payload)
	}
	if got := stub.queryCount(); got != 4 {
		t.Fatalf("stale cached address was not evicted and re-resolved, queries = %d", got)
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
	if stub.queryCount() == 0 {
		t.Fatal("configured nameserver received no dns query")
	}
}

func TestProxy_ReloadNameservers(t *testing.T) {
	stub := startDNSStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	refused := "127.0.0.1:" + closedUDPPort(t)
	config := "servers:\n" +
		"  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: http://unused\n" +
		"nameservers:\n  - \"" + refused + "\"\n"
	port := testPort(t)
	servers := testNameservers(t)
	path := writeConfigFile(t, "r.yaml", withProxyPort(config, port))
	p, err := newProxy(path, newTransport(servers.DialContext), nil, nil, servers)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	t.Cleanup(func() { p.listeners.closeAll() })
	if got := servers.serverAddresses(); !slices.Equal(got, []string{refused}) {
		t.Fatalf("initial nameservers = %v, want %v", got, []string{refused})
	}
	if err := os.WriteFile(path, []byte(withProxyPort(strings.Replace(config, refused, stub.address(), 1), port)), 0o644); err != nil {
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

// silentUDPPort 返回一个只收包、永不回复的 UDP 端口，用于模拟不可达的 DNS 服务器。
func silentUDPPort(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return strconv.Itoa(conn.LocalAddr().(*net.UDPAddr).Port)
}

type dnsStub struct {
	listener *net.UDPConn
	mu       sync.Mutex
	records  map[uint16][]net.IP
	ttl      uint16
	queries  atomic.Int32
}

func (s *dnsStub) setRecords(records map[uint16][]net.IP) {
	s.mu.Lock()
	s.records = records
	s.mu.Unlock()
}

func startDNSStub(t *testing.T, records map[uint16][]net.IP) *dnsStub {
	return startDNSStubWithTTL(t, records, 0)
}

func startDNSStubWithTTL(t *testing.T, records map[uint16][]net.IP, ttl uint16) *dnsStub {
	t.Helper()
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	stub := &dnsStub{listener: listener.(*net.UDPConn), records: records, ttl: ttl}
	t.Cleanup(func() { _ = stub.listener.Close() })
	go stub.serve()
	return stub
}

func (s *dnsStub) address() string {
	return s.listener.LocalAddr().String()
}

func (s *dnsStub) queryCount() int {
	return int(s.queries.Load())
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
		s.mu.Lock()
		reply := dnsReply(id, name, qtype, s.records[qtype], s.ttl)
		s.mu.Unlock()
		s.queries.Add(1)
		_, _ = s.listener.WriteTo(reply, remote)
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

func dnsReply(id uint16, name string, qtype uint16, records []net.IP, ttl uint16) []byte {
	header := make([]byte, 12)
	binary.BigEndian.PutUint16(header[0:2], id)
	binary.BigEndian.PutUint16(header[2:4], 0x8180)
	binary.BigEndian.PutUint16(header[4:6], 1)
	binary.BigEndian.PutUint16(header[6:8], uint16(len(records)))
	binary.BigEndian.PutUint16(header[8:10], 0)
	binary.BigEndian.PutUint16(header[10:12], 0)
	reply := append(header, dnsQuestion(name, qtype)...)
	for _, record := range records {
		reply = append(reply, dnsAnswer(qtype, record, ttl)...)
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

func dnsAnswer(qtype uint16, address net.IP, ttl uint16) []byte {
	answer := make([]byte, 12)
	binary.BigEndian.PutUint16(answer[0:2], 0xc00c)
	binary.BigEndian.PutUint16(answer[2:4], qtype)
	binary.BigEndian.PutUint16(answer[4:6], 1)
	binary.BigEndian.PutUint16(answer[6:8], ttl)
	rdata := address.To16()
	if v4 := address.To4(); v4 != nil {
		rdata = v4
	}
	binary.BigEndian.PutUint16(answer[10:12], uint16(len(rdata)))
	return append(answer, rdata...)
}

func writeCertFiles(t *testing.T, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) (string, string) {
	t.Helper()
	certPath := writeConfigFile(t, "ca.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})))
	key, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := writeConfigFile(t, "ca.key", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: key})))
	return certPath, keyPath
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// syncBuffer 供 os/exec 的管道拷贝 goroutine 写入、测试主 goroutine 读取
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestE2E_ConfigNameserversTakeEffect 用真实 mrp 二进制验证配置文件里的 nameservers 字段驱动上游解析
func TestE2E_ConfigNameserversTakeEffect(t *testing.T) {
	binary, err := exec.LookPath("./mrp")
	if err != nil {
		t.Skip("mrp binary not built")
	}
	up := recordingServer(t, "upstream")
	_, upPort, err := net.SplitHostPort(up.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	stub := startDNSStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	listenPort := freePort(t)
	configPath := writeConfigFile(t, "e2e.yaml",
		"servers:\n  - domain: api.example.com\n    port: "+listenPort+"\n    routes:\n      - prefix: /\n        upstream: http://api.mrp.local:"+upPort+"\n"+
			"nameservers:\n  - \"127.0.0.1:"+closedUDPPort(t)+"\"\n  - \""+stub.address()+"\"\n")
	caCert, caKey := testAuthorityCA(t)
	certPath, keyPath := writeCertFiles(t, caCert, caKey)

	var logs syncBuffer
	cmd := exec.Command(binary, "--config", configPath, "--cert", certPath, "--key", keyPath, "--log", "debug")
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	for range 200 {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+listenPort, time.Second)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(mustURL("http://127.0.0.1:" + listenPort)),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	var body string
	for range 100 {
		request, err := http.NewRequest("GET", "http://api.example.com/hello", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			body = err.Error()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		var buffer bytes.Buffer
		_, _ = io.Copy(&buffer, response.Body)
		_ = response.Body.Close()
		body = buffer.String()
		if strings.HasPrefix(body, "upstream:/hello") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.HasPrefix(body, "upstream:/hello") {
		t.Fatalf("body = %q\nlog:\n%s", body, logs.String())
	}
	if stub.queryCount() == 0 {
		t.Fatalf("configured nameserver received no dns query\nlog:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "dns nameservers=") {
		t.Fatalf("effective nameservers not logged\nlog:\n%s", logs.String())
	}
}
