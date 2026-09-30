package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"mrp/internal/ca"
	"mrp/internal/config"
	"mrp/internal/dns"
	"mrp/internal/logging"
	"mrp/internal/server"
	"mrp/internal/testutil"
)

func init() {
	logging.SetOutput(io.Discard)
}

func testNameservers(t *testing.T, entries ...string) *dns.Resolver {
	t.Helper()
	return testNameserversWithTimeout(t, dns.AttemptTimeout, dns.TTL, entries)
}

func testNameserversWithTimeout(t *testing.T, attemptTimeout, dnsTTL time.Duration, entries []string) *dns.Resolver {
	t.Helper()
	servers, err := dns.New(attemptTimeout, dnsTTL, entries)
	if err != nil {
		t.Fatalf("dns.New: %v", err)
	}
	return servers
}

func recordingServer(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, tag+":"+request.URL.Path+"|host="+request.Host)
	}))
	t.Cleanup(server.Close)
	return server
}

func startProxy(t *testing.T, configPath string, tlsConfig *tls.Config) (string, *Proxy) {
	t.Helper()
	return startProxyWithNameservers(t, configPath, tlsConfig, testNameservers(t))
}

// startProxyWithNameservers 用指定的解析器构建代理，并把路由处理器预挂到随机端口
func startProxyWithNameservers(t *testing.T, configPath string, tlsConfig *tls.Config, servers *dns.Resolver) (string, *Proxy) {
	t.Helper()
	proxy, err := New(configPath, NewTransport(servers.DialContext), tlsConfig, nil, servers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return startListener(t, proxy), proxy
}

// startListener 绑定随机空闲端口并开始服务，返回可直接访问的代理地址
func startListener(t *testing.T, proxy *Proxy) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go server.Serve(listener, proxy.Handler())
	return "http://" + listener.Addr().String()
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

// getOnce 请求一次并立即关闭响应体，使上游连接回到空闲池以便观测连接复用
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

func TestRouteTable_InstallHandlers(t *testing.T) {
	table, _, err := config.Load(testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n"+
			"      - prefix: /\n        upstream: http://up-a:8080\n"+
			"      - prefix: /api/\n        upstream: http://up-b:8080\n"+
			"        headers:\n          response:\n            X-Test: \"1\"\n"+
			"      - prefix: /files/\n        upstream: ./dist\n"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	table.InstallHandlers(NewTransport(nil))

	remote, ok := table.Pick("http", 0, "api.example.com", "/v1")
	if !ok || remote.Handler() == nil {
		t.Fatalf("remote route must get a handler: %+v ok=%v", remote, ok)
	}
	proxyHandler, ok := remote.Handler().(*httputil.ReverseProxy)
	if !ok {
		t.Fatalf("remote route handler: got %T", remote.Handler())
	} else if proxyHandler.ModifyResponse != nil {
		t.Fatal("route without response headers must not mount a modify hook")
	}
	hooked, ok := table.Pick("http", 0, "api.example.com", "/api/x")
	if !ok {
		t.Fatal("/api/ prefix should match")
	}
	proxyHandler, ok = hooked.Handler().(*httputil.ReverseProxy)
	if !ok || proxyHandler.ModifyResponse == nil {
		t.Fatalf("route with response headers must mount a modify hook: %T", hooked.Handler())
	}
	files, ok := table.Pick("http", 0, "api.example.com", "/files/a")
	if !ok {
		t.Fatal("/files/ prefix should match")
	}
	if _, ok := files.Handler().(*httputil.ReverseProxy); ok {
		t.Fatalf("local route must not be served by the reverse proxy: %T", files.Handler())
	}
}

// upstream 写成 "." 与 "./" 必须都按本地目录托管，不得被当作远程主机代理
func TestRouteTable_DotUpstreamIsLocal(t *testing.T) {
	for _, upstream := range []string{".", "./"} {
		table, _, err := config.Load(testutil.ConfigFile(t, "dot.yaml",
			"servers:\n  - domain: site.example.com\n    routes:\n"+
				"      - prefix: /\n        upstream: "+upstream+"\n"))
		if err != nil {
			t.Fatalf("config.Load upstream=%q: %v", upstream, err)
		}
		table.InstallHandlers(NewTransport(nil))
		matched, ok := table.Pick("http", 0, "site.example.com", "/")
		if !ok {
			t.Fatalf("upstream %q: / should match", upstream)
		}
		if _, ok := matched.Handler().(*httputil.ReverseProxy); ok {
			t.Fatalf("upstream %q must be a local directory, got %T", upstream, matched.Handler())
		}
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
	proxyURL, _ := startProxy(t, testutil.ConfigFile(t, "r.yaml", content), nil)
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
	if runtime.GOOS == "windows" {
		t.Skip("local directory routing is not exercised on windows")
	}
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
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: static.example.com\n    routes:\n      - prefix: /\n        upstream: "+root+"\n")
	proxyURL, _ := startProxy(t, configPath, nil)
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
	if runtime.GOOS == "windows" {
		t.Skip("local directory routing is not exercised on windows")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: assets.example.com\n    routes:\n      - prefix: /assets/\n        upstream: "+root+"\n")
	proxyURL, _ := startProxy(t, configPath, nil)
	client := proxyClient(proxyURL)

	if got := requestBody(t, client, "http://assets.example.com/assets/data.txt"); got != "hello" {
		t.Fatalf("prefix mapping: got %q", got)
	}
}

func TestProxy_StaticAndRemoteCoexist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local directory routing is not exercised on windows")
	}
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
	proxyURL, _ := startProxy(t, testutil.ConfigFile(t, "r.yaml", content), nil)
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
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+up.URL+"\n        headers:\n          response:\n            Access-Control-Allow-Origin: \"*\"\n            X-Injected: mrp\n")
	proxyURL, _ := startProxy(t, configPath, nil)
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
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+up.URL+"\n        headers:\n          request:\n            Authorization: \"Bearer secret\"\n            X-Trace: mrp\n")
	proxyURL, _ := startProxy(t, configPath, nil)
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
	cert := testutil.SelfSignedCert(t, []string{"api.example.com"})
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12}
	content := "servers:\n" +
		"  - domain: api.example.com\n" +
		"    routes:\n" +
		"      - prefix: /\n" +
		"        upstream: " + up.URL + "\n"
	proxyURL, _ := startProxy(t, testutil.ConfigFile(t, "r.yaml", content), tlsConfig)
	client := proxyClient(proxyURL)

	got := requestBody(t, client, "https://api.example.com/v1/data")
	if got != "TLS:/v1/data" {
		t.Fatalf("https via connect: got %q", got)
	}
}

// protocol 只作为虚拟服务器匹配条件：声明 protocol: https 的条目只接 CONNECT，
// 同域名的明文 GET 不命中该条目而走透传
func TestProxy_ProtocolMatchesConnectOnly(t *testing.T) {
	up := recordingServer(t, "MATCHED")
	content := "servers:\n" +
		"  - domain: api.example.com\n" +
		"    protocol: https\n" +
		"    routes:\n" +
		"      - prefix: /\n" +
		"        upstream: " + up.URL + "\n"
	configPath := testutil.ConfigFile(t, "r.yaml", content)

	// 未配 CA 时 CONNECT 无法 MITM，命中条目也回 502
	proxyURL, _ := startProxy(t, configPath, nil)
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n")
	if code := readConnectResponse(t, bufio.NewReader(conn)); code != http.StatusBadGateway {
		t.Fatalf("want 502 without ca, got %d", code)
	}

	caCert, caKey := testutil.AuthorityCA(t)
	tlsConfig := &tls.Config{
		GetCertificate: ca.New(caCert, caKey).GetCertificate,
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}
	proxyURL, _ = startProxy(t, configPath, tlsConfig)
	client := proxyClient(proxyURL)

	resp, err := client.Get("http://api.example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("plaintext get should miss https entry: got %d", resp.StatusCode)
	}

	if got := requestBody(t, client, "https://api.example.com/x"); !strings.HasPrefix(got, "MATCHED:/x") {
		t.Fatalf("https via connect: got %q", got)
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
	proxyURL, _ := startProxy(t, testutil.ConfigFile(t, "r.yaml", content), nil)
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

// MITM 按 CONNECT 目标域名签发证书：客户端以 CA 为信任根校验通过
func TestProxy_MITMSignsCertForTarget(t *testing.T) {
	up := recordingServer(t, "SNI")
	caCert, caKey := testutil.AuthorityCA(t)
	tlsConfig := &tls.Config{
		GetCertificate: ca.New(caCert, caKey).GetCertificate,
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}
	configPath := testutil.ConfigFile(t, "r.yaml", "servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+up.URL+"\n")
	proxyURL, _ := startProxy(t, configPath, tlsConfig)

	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(testutil.MustURL(proxyURL)),
			TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "api.example.com"},
		},
	}
	if got := requestBody(t, client, "https://api.example.com/v1/data"); !strings.HasPrefix(got, "SNI:/v1/data") {
		t.Fatalf("mitm: got %q", got)
	}
}

func TestProxy_UpstreamResolvedByNameserver(t *testing.T) {
	up := recordingServer(t, "upstream")
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	stub := testutil.NewStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	content := "servers:\n" +
		"  - domain: api.example.com\n" +
		"    routes:\n" +
		"      - prefix: /\n" +
		"        upstream: http://api.mrp.local:" + port + "\n" +
		"nameservers:\n" +
		"  - \"127.0.0.1:" + testutil.ClosedUDPPort(t) + "\"\n" +
		"  - \"" + stub.Address() + "\"\n"
	proxyURL, _ := startProxy(t, testutil.ConfigFile(t, "r.yaml", content), nil)
	if got := requestBody(t, proxyClient(proxyURL), "http://api.example.com/hello"); !strings.HasPrefix(got, "upstream:/hello") {
		t.Fatalf("nameserver routing: got %q", got)
	}
	if stub.QueryCount() == 0 {
		t.Fatal("configured nameserver received no dns query")
	}
}

func TestProxy_ReloadNameservers(t *testing.T) {
	stub := testutil.NewStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	refused := "127.0.0.1:" + testutil.ClosedUDPPort(t)
	content := "servers:\n" +
		"  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: http://unused\n" +
		"nameservers:\n  - \"" + refused + "\"\n"
	servers := testNameservers(t)
	configPath := testutil.ConfigFile(t, "r.yaml", content)
	proxy, err := New(configPath, NewTransport(servers.DialContext), nil, nil, servers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := servers.Addresses(); !slices.Equal(got, []string{refused}) {
		t.Fatalf("initial nameservers = %v, want %v", got, []string{refused})
	}
	if err := os.WriteFile(configPath, []byte(strings.Replace(content, refused, stub.Address(), 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := proxy.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := servers.Addresses(); !slices.Equal(got, []string{stub.Address()}) {
		t.Fatalf("reloaded nameservers = %v, want %v", got, []string{stub.Address()})
	}
}
