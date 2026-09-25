package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	t.Helper()
	transportVerify := http.DefaultTransport.(*http.Transport).Clone()
	transportVerify.Proxy = nil
	transportVerify.DialContext = (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	transportVerify.ResponseHeaderTimeout = 2 * time.Second
	transportInsecure := transportVerify.Clone()
	transportInsecure.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	p, err := newProxy(configPath, transportVerify, transportInsecure, tlsConfig)
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
        tls_verify: false
`)
	table, err := loadTable(path)
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
	if entry, ok := table.pick("b.example.com", "/"); !ok || !entry.insecure {
		t.Fatalf("insecure not applied: %+v ok=%v", entry, ok)
	}
}

func TestLoadTable_Errors(t *testing.T) {
	cases := map[string]string{
		"empty_domain": "servers:\n  - domain: \"\"\n    routes: []",
		"dup_domain":   "servers:\n  - domain: a\n    routes: []\n  - domain: a\n    routes: []",
		"empty_prefix": "servers:\n  - domain: a\n    routes:\n      - prefix: \"\"\n        upstream: http://x",
		"dup_prefix":   "servers:\n  - domain: a\n    routes:\n      - prefix: /a\n        upstream: http://x\n      - prefix: /a\n        upstream: http://y",
		"bad_upstream": "servers:\n  - domain: a\n    routes:\n      - prefix: /\n        upstream: ftp://x",
		"bad_yaml":     "servers: [this is broken",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfigFile(t, name+".yaml", content)
			if _, err := loadTable(path); err == nil {
				t.Fatalf("expected error for %s", name)
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
		},
	}}
	cases := map[string]string{
		"/v1/users/1": "/v1/users/",
		"/v1/other":   "/v1/",
		"/other":      "/",
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
		"        upstream: " + up.URL + "\n" +
		"        tls_verify: false\n"
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
	path := filepath.Join(t.TempDir(), "routing.yaml")
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
	cfg, err := resolveTLSConfig(certPath, keyPath, false, false)
	if err != nil || cfg != nil {
		t.Fatalf("want nil config without certs, got %v err=%v", cfg, err)
	}
	if err := os.WriteFile(certPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveTLSConfig(certPath, keyPath, false, false); err == nil {
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

	if _, err := authority.getCertificate(&tls.ClientHelloInfo{ServerName: ""}); err == nil {
		t.Fatal("缺少 SNI 应报错")
	}
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
