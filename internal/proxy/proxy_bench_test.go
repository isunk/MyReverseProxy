package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"mrp/internal/ca"
	"mrp/internal/dns"
	"mrp/internal/testutil"
)

const (
	benchClientDomain = "api.bench.local"
	benchUpstreamHost = "up.bench.local"
	typeA             = 1
)

type benchEnv struct {
	proxyURL    string
	client      *http.Client
	authority   *ca.Authority
	upstreamURL string
	upstreamCA  *x509.Certificate
}

// newBenchEnv 搭一条真实的 MITM 链路：客户端 CONNECT 到虚拟域名，代理解密后
// 转发到需经 DNS 解析的 HTTPS 上游，客户端对 mrp 签发证书做真实校验。
func newBenchEnv(t testing.TB) *benchEnv {
	t.Helper()

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)

	parsed, _ := url.Parse(upstream.URL)
	_, port, _ := net.SplitHostPort(parsed.Host)

	caCert, caKey := testutil.AuthorityCA(t)
	certPath, keyPath := testutil.WriteCertFiles(t, caCert, caKey)
	tlsConfig, authority, err := ca.Load(certPath, keyPath, true)
	if err != nil || authority == nil {
		t.Fatalf("ca.Load: %v", err)
	}

	stub := testutil.NewStub(t, map[uint16][]net.IP{typeA: {net.ParseIP("127.0.0.1")}})
	nameservers := testNameserversWithTimeout(t, dns.AttemptTimeout, dns.TTL, []string{stub.Address()})

	configPath := testutil.ConfigFile(t, "bench.yaml", fmt.Sprintf(`
servers:
  - domain: %s
    routes:
      - prefix: /
        upstream: https://%s:%s
nameservers:
  - %s
`, benchClientDomain, benchUpstreamHost, port, stub.Address()))

	proxy, err := New(configPath, NewTransport(nameservers.DialContext), tlsConfig, authority, nameservers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	proxyURL := startListener(t, proxy)
	return &benchEnv{
		proxyURL:    proxyURL,
		authority:   authority,
		upstreamURL: upstream.URL,
		upstreamCA:  upstream.Certificate(),
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyURL(testutil.MustURL(proxyURL)),
				MaxIdleConns:        8,
				MaxIdleConnsPerHost: 4,
				TLSClientConfig:     &tls.Config{RootCAs: benchPool(caCert)},
			},
		},
	}
}

// benchPool 把单张证书做成信任池，客户端对 mrp 签发证书与上游证书都走真实校验。
func benchPool(cert *x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return pool
}

// getAndDrain 请求一次并读完响应体，读完才能归还连接进池，否则每次都重建连接。
func getAndDrain(t testing.TB, client *http.Client, target string) string {
	t.Helper()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func benchRequest(t testing.TB, client *http.Client) string {
	return getAndDrain(t, client, "https://"+benchClientDomain+"/")
}

// BenchmarkMITM_Warm 稳态每请求耗时：证书已签发、上游连接在池内、TLS 会话可复用。
func BenchmarkMITM_Warm(b *testing.B) {
	b.ReportAllocs()
	env := newBenchEnv(b)
	if body := benchRequest(b, env.client); body != "ok" {
		b.Fatalf("warmup body = %q", body)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if body := benchRequest(b, env.client); body != "ok" {
			b.Fatalf("body = %q", body)
		}
	}
}

// BenchmarkDirect_Warm 同一条上游、同样的证书校验，只是不走 mrp，用作 mrp 自身开销的对照基线。
func BenchmarkDirect_Warm(b *testing.B) {
	b.ReportAllocs()
	env := newBenchEnv(b)
	direct := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        8,
			MaxIdleConnsPerHost: 4,
			TLSClientConfig:     &tls.Config{RootCAs: benchPool(env.upstreamCA)},
		},
	}
	target := env.upstreamURL
	if getAndDrain(b, direct, target) != "ok" {
		b.Fatal("warmup body mismatch")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if body := getAndDrain(b, direct, target); body != "ok" {
			b.Fatalf("body = %q", body)
		}
	}
}

// BenchmarkTransport_POST_ExpectContinue 客户端带 Expect: 100-continue 的上行请求。
// Go 服务端读请求体会自动回 100，所以正常配置下这项保持在毫秒级；
// 若上游读取 body 却不回 100，Transport 会白等满 ExpectContinueTimeout。
func BenchmarkTransport_POST_ExpectContinue(b *testing.B) {
	b.ReportAllocs()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		_, _ = writer.Write([]byte("ok"))
	}))
	b.Cleanup(upstream.Close)

	client := &http.Client{Transport: NewTransport(nil)}
	payload := bytes.Repeat([]byte("x"), 8<<10)
	for i := 0; i < b.N; i++ {
		if got := postExpectContinue(b, client, upstream.URL, payload); got != "ok" {
			b.Fatalf("body = %q", got)
		}
	}
}

// postExpectContinue 发一个带 Expect: 100-continue 的 POST，写完请求体并读完响应。
func postExpectContinue(t testing.TB, client *http.Client, target string, payload []byte) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Expect", "100-continue")
	resp, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// TestMITM_ColdLatency 量化首次请求的代价：证书未签发、DNS 未解析、上游连接为冷。
// 只跑一次，用来判断启动预热是否值得做。
func TestMITM_ColdLatency(t *testing.T) {
	env := newBenchEnv(t)

	env.authority.ClearCache()
	start := time.Now()
	if body := benchRequest(t, env.client); body != "ok" {
		t.Fatalf("body = %q", body)
	}
	first := time.Since(start)
	t.Logf("首次请求（签发证书 + 解析上游 + 建立上游连接）: %s", first)

	for i := 1; i <= 6; i++ {
		env.authority.ClearCache()
		env.client.Transport.(*http.Transport).CloseIdleConnections()
		start = time.Now()
		if body := benchRequest(t, env.client); body != "ok" {
			t.Fatalf("body = %q", body)
		}
		t.Logf("第 %d 次冷请求（清证书 + 拆上游连接）: %s", i, time.Since(start))
	}

	signStart := time.Now()
	for i := 0; i < 10; i++ {
		if _, err := env.authority.GetCertificate(&tls.ClientHelloInfo{ServerName: fmt.Sprintf("sign%d.bench.local", i)}); err != nil {
			t.Fatalf("GetCertificate: %v", err)
		}
	}
	t.Logf("单次签发证书平均: %s", time.Since(signStart)/10)
}

// BenchmarkTunnel_Copy 未命中域名的透传搬运速率：mrp 只搬字节，不做解密不改写。
// 报告每秒搬运字节数，用来判断隧道缓冲是否需要加大。
func BenchmarkTunnel_Copy(b *testing.B) {
	target := testutil.EchoServer(b)

	configPath := testutil.ConfigFile(b, "tunnel.yaml", "servers: []\n")
	proxy, err := New(configPath, NewTransport(nil), nil, nil, testNameservers(b))
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	proxyURL := startListener(b, proxy)

	payload := bytes.Repeat([]byte("x"), 256<<10)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if moved := tunnelCopy(b, proxyURL, target, payload); moved != int64(len(payload)) {
			b.Fatalf("moved %d bytes, want %d", moved, len(payload))
		}
	}
}

// tunnelCopy 经代理建一条 CONNECT 隧道，写入 payload 并读回等量字节，返回读回字节数。
// 用 ReadFull 而非读到 EOF：mrp 的隧道在客户端关闭写端后会立刻关掉上游连接，
// 半关闭场景下上游回传的字节会被截断，不代表正常请求路径。
func tunnelCopy(b *testing.B, proxyURL, target string, payload []byte) int64 {
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		b.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		b.Fatal(err)
	}
	if _, err := conn.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n")); err != nil {
		b.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		b.Fatalf("connect reply = %q, err = %v", status, err)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		b.Fatal(err)
	}

	if _, err := conn.Write(payload); err != nil {
		b.Fatal(err)
	}
	moved, err := io.CopyN(io.Discard, reader, int64(len(payload)))
	if err != nil {
		b.Fatalf("read echoed payload: moved=%d err=%v", moved, err)
	}
	return moved
}

// TestDNS_FallbackLatency 量化首台 DNS 服务器不可用时冷解析的代价：
// 解析按配置顺序逐台尝试，前一台要等满单次超时才切换。
func TestDNS_FallbackLatency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		io.WriteString(writer, "ok")
	}))
	defer server.Close()

	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	records := map[uint16][]net.IP{typeA: {net.ParseIP("127.0.0.1")}}
	target := net.JoinHostPort("up."+benchUpstreamHost, port)

	for _, label := range []string{"首台 DNS 静默不回复", "首台 DNS 直接拒绝"} {
		stub := testutil.NewStub(t, records)
		var resolver *dns.Resolver
		if label == "首台 DNS 直接拒绝" {
			resolver, err = dns.New(dns.AttemptTimeout, dns.TTL, []string{"127.0.0.1:" + testutil.ClosedUDPPort(t), stub.Address()})
		} else {
			resolver, err = dns.New(dns.AttemptTimeout, dns.TTL, []string{"127.0.0.1:" + testutil.SilentUDPPort(t), stub.Address()})
		}
		if err != nil {
			t.Fatalf("dns.New: %v", err)
		}

		start := time.Now()
		conn, err := resolver.DialContext(context.Background(), "tcp", target)
		if err != nil {
			t.Fatalf("DialContext: %v", err)
		}
		_ = conn.Close()
		t.Logf("%s 后切换，冷解析耗时: %s", label, time.Since(start).Round(time.Millisecond))
	}
}
