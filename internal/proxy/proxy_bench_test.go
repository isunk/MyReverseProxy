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

// newBenchEnv 搭一条真实的 MITM 链路：客户端 CONNECT 到虚拟域名，代理解密后转发到需经 DNS 解析的 HTTPS 上游，客户端对 mrp 签发证书做真实校验。
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

// BenchmarkHTTP_Warm 稳态 HTTP 代理每请求耗时：无 TLS 握手、无 MITM，纯路由转发到 HTTP 上游，对照 BenchmarkMITM_Warm 量化 MITM 自身开销。
func BenchmarkHTTP_Warm(b *testing.B) {
	b.ReportAllocs()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("ok"))
	}))
	b.Cleanup(upstream.Close)

	parsed, _ := url.Parse(upstream.URL)
	_, port, _ := net.SplitHostPort(parsed.Host)

	configPath := testutil.ConfigFile(b, "http-bench.yaml", fmt.Sprintf(`
servers:
  - domain: %s
    routes:
      - prefix: /
        upstream: http://127.0.0.1:%s
`, benchClientDomain, port))

	proxy, err := New(configPath, NewTransport(nil), nil, nil, testNameservers(b))
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	proxyURL := startListener(b, proxy)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(testutil.MustURL(proxyURL)),
			MaxIdleConns:        8,
			MaxIdleConnsPerHost: 4,
		},
	}

	target := "http://" + benchClientDomain + "/"
	if body := getAndDrain(b, client, target); body != "ok" {
		b.Fatalf("warmup body = %q", body)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if body := getAndDrain(b, client, target); body != "ok" {
			b.Fatalf("body = %q", body)
		}
	}
}

// BenchmarkTransport_POST_ExpectContinue 客户端带 Expect: 100-continue 的上行请求。Go 服务端读请求体会自动回 100，所以正常配置下这项保持在毫秒级；若上游读取 body 却不回 100，Transport 会白等满 ExpectContinueTimeout。
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

// TestMITM_ColdLatency 量化首次请求的代价：证书未签发、DNS 未解析、上游连接为冷。只跑一次，用来判断启动预热是否值得做。
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

// BenchmarkTunnel_Copy 未命中域名的透传搬运速率：mrp 只搬字节，不做解密不改写。报告每秒搬运字节数，用来判断隧道缓冲是否需要加大。
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
func tunnelCopy(b *testing.B, proxyURL, target string, payload []byte) int64 {
	moved, err := tunnelUpload(strings.TrimPrefix(proxyURL, "http://"), target, payload)
	if err != nil {
		b.Fatalf("tunnel copy: %v", err)
	}
	return moved
}

// tunnelUpload 经代理 CONNECT 目标并写入 payload、读回等量字节。用 CopyN 读等量而非读到 EOF：mrp 的隧道在客户端关闭写端后会立刻关掉上游连接，半关闭场景下上游回传的字节会被截断，不代表正常请求路径。
func tunnelUpload(proxyAddr, target string, payload []byte) (int64, error) {
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return 0, err
	}
	if _, err := conn.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n")); err != nil {
		return 0, err
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		return 0, fmt.Errorf("connect reply = %q, err = %v", status, err)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		return 0, err
	}

	if _, err := conn.Write(payload); err != nil {
		return 0, err
	}
	return io.CopyN(io.Discard, reader, int64(len(payload)))
}

// TestDNS_FallbackLatency 量化首台 DNS 服务器不可用时冷解析的代价：解析按配置顺序逐台尝试，前一台要等满单次超时才切换。
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

// ---- 上传吞吐基准：直连 / HTTP 代理 / 隧道 三场景，按请求体积与并发两个维度压测 ----

// uploadHarness 搭一条上传链路：HTTP 上游读 body 丢弃并回 "ok"，另起一个原始回显服务供隧道搬运；直连与代理共用同一上游。
type uploadHarness struct {
	directClient *http.Client
	proxyClient  *http.Client
	directURL    string
	proxyTarget  string
	proxyAddr    string
	echoAddr     string
}

func newUploadHarness(b *testing.B) *uploadHarness {
	return newUploadHarnessBuffer(b, transportBufferSize)
}

// newUploadHarnessBuffer 同 newUploadHarness，但可指定上游传输层读写缓冲大小，用于对比 buffer 尺寸对上行吞吐的影响。
func newUploadHarnessBuffer(b *testing.B, bufSize int) *uploadHarness {
	b.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		_, _ = writer.Write([]byte("ok"))
	}))
	b.Cleanup(upstream.Close)

	parsed, _ := url.Parse(upstream.URL)
	_, port, _ := net.SplitHostPort(parsed.Host)

	configPath := testutil.ConfigFile(b, "upload-bench.yaml", fmt.Sprintf(`
servers:
  - domain: %s
    routes:
      - prefix: /
        upstream: http://127.0.0.1:%s
`, benchClientDomain, port))

	transport := NewTransport(nil)
	transport.ReadBufferSize = bufSize
	transport.WriteBufferSize = bufSize
	proxyInstance, err := New(configPath, transport, nil, nil, testNameservers(b))
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	proxyURL := startListener(b, proxyInstance)

	return &uploadHarness{
		directClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{MaxIdleConns: 256, MaxIdleConnsPerHost: 64},
		},
		proxyClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyURL(testutil.MustURL(proxyURL)),
				MaxIdleConns:        256,
				MaxIdleConnsPerHost: 64,
			},
		},
		directURL:   upstream.URL,
		proxyTarget: "http://" + benchClientDomain + "/",
		proxyAddr:   strings.TrimPrefix(proxyURL, "http://"),
		echoAddr:    testutil.EchoServer(b),
	}
}

// postDrain 上传 payload 到 target 并读完响应，读完才能归还连接进池。
func postDrain(client *http.Client, target string, payload []byte) error {
	resp, err := client.Post(target, "application/octet-stream", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

// BenchmarkUpload 单流上传吞吐：按请求体积（流量）分档，直连/代理/隧道三场景对照，b.SetBytes 报告 MB/s。
func BenchmarkUpload(b *testing.B) {
	harness := newUploadHarness(b)
	sizes := []struct {
		name string
		size int
	}{
		{"1KB", 1 << 10},
		{"1MB", 1 << 20},
		{"16MB", 16 << 20},
	}
	for _, entry := range sizes {
		size := entry.size
		b.Run(entry.name+"/direct", func(b *testing.B) {
			benchUploadDirect(b, harness.directClient, harness.directURL, size)
		})
		b.Run(entry.name+"/proxy", func(b *testing.B) {
			benchUploadProxy(b, harness.proxyClient, harness.proxyTarget, size)
		})
		b.Run(entry.name+"/tunnel", func(b *testing.B) {
			benchUploadTunnel(b, harness.proxyAddr, harness.echoAddr, size)
		})
	}
}

// BenchmarkUploadParallel 并发上传吞吐：固定 1MB 请求体，按并发度分档，报告聚合 MB/s。
func BenchmarkUploadParallel(b *testing.B) {
	harness := newUploadHarness(b)
	const size = 1 << 20
	for _, parallelism := range []int{1, 4, 16} {
		label := fmt.Sprintf("p%d", parallelism)
		b.Run(label+"/direct", func(b *testing.B) {
			benchUploadParallelDirect(b, harness.directClient, harness.directURL, size, parallelism)
		})
		b.Run(label+"/proxy", func(b *testing.B) {
			benchUploadParallelProxy(b, harness.proxyClient, harness.proxyTarget, size, parallelism)
		})
		b.Run(label+"/tunnel", func(b *testing.B) {
			benchUploadParallelTunnel(b, harness.proxyAddr, harness.echoAddr, size, parallelism)
		})
	}
}

func benchUploadDirect(b *testing.B, client *http.Client, target string, size int) {
	payload := bytes.Repeat([]byte("x"), size)
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := postDrain(client, target, payload); err != nil {
			b.Fatalf("direct upload: %v", err)
		}
	}
}

func benchUploadProxy(b *testing.B, client *http.Client, target string, size int) {
	payload := bytes.Repeat([]byte("x"), size)
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := postDrain(client, target, payload); err != nil {
			b.Fatalf("proxy upload: %v", err)
		}
	}
}

func benchUploadTunnel(b *testing.B, proxyAddr, echoAddr string, size int) {
	payload := bytes.Repeat([]byte("x"), size)
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		moved, err := tunnelUpload(proxyAddr, echoAddr, payload)
		if err != nil {
			b.Fatalf("tunnel upload: %v", err)
		}
		if moved != int64(size) {
			b.Fatalf("tunnel moved %d bytes, want %d", moved, size)
		}
	}
}

func benchUploadParallelDirect(b *testing.B, client *http.Client, target string, size, parallelism int) {
	payload := bytes.Repeat([]byte("x"), size)
	b.SetBytes(int64(size))
	b.SetParallelism(parallelism)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := postDrain(client, target, payload); err != nil {
				b.Errorf("direct upload: %v", err)
				return
			}
		}
	})
}

func benchUploadParallelProxy(b *testing.B, client *http.Client, target string, size, parallelism int) {
	payload := bytes.Repeat([]byte("x"), size)
	b.SetBytes(int64(size))
	b.SetParallelism(parallelism)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := postDrain(client, target, payload); err != nil {
				b.Errorf("proxy upload: %v", err)
				return
			}
		}
	})
}

func benchUploadParallelTunnel(b *testing.B, proxyAddr, echoAddr string, size, parallelism int) {
	payload := bytes.Repeat([]byte("x"), size)
	b.SetBytes(int64(size))
	b.SetParallelism(parallelism)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			moved, err := tunnelUpload(proxyAddr, echoAddr, payload)
			if err != nil {
				b.Errorf("tunnel upload: %v", err)
				return
			}
			if moved != int64(size) {
				b.Errorf("tunnel moved %d bytes, want %d", moved, size)
				return
			}
		}
	})
}

// BenchmarkUploadBuffer 对比上游传输层缓冲大小对代理上行吞吐的影响，固定 16MB 请求体。
func BenchmarkUploadBuffer(b *testing.B) {
	for _, bufSize := range []int{32 << 10, 128 << 10, 256 << 10, 1 << 20} {
		label := fmt.Sprintf("%dKB", bufSize>>10)
		b.Run(label, func(b *testing.B) {
			harness := newUploadHarnessBuffer(b, bufSize)
			benchUploadProxy(b, harness.proxyClient, harness.proxyTarget, 16<<20)
		})
	}
}

// mitmUploadHarness 搭一条 HTTPS 上传链路：上游读 body 丢弃；直连、隧道、MITM 三种客户端共用同一 HTTPS 上游，用于侧写四种转发模式的开销。
type mitmUploadHarness struct {
	mitmClient   *http.Client
	directClient *http.Client
	tunnelClient *http.Client
	upstreamURL  string
	proxyTarget  string
}

func newUploadMITMHarness(b *testing.B) *mitmUploadHarness {
	b.Helper()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		_, _ = writer.Write([]byte("ok"))
	}))
	b.Cleanup(upstream.Close)

	parsed, _ := url.Parse(upstream.URL)
	_, port, _ := net.SplitHostPort(parsed.Host)

	caCert, caKey := testutil.AuthorityCA(b)
	certPath, keyPath := testutil.WriteCertFiles(b, caCert, caKey)
	tlsConfig, authority, err := ca.Load(certPath, keyPath, true)
	if err != nil || authority == nil {
		b.Fatalf("ca.Load: %v", err)
	}

	stub := testutil.NewStub(b, map[uint16][]net.IP{typeA: {net.ParseIP("127.0.0.1")}})
	nameservers := testNameserversWithTimeout(b, dns.AttemptTimeout, dns.TTL, []string{stub.Address()})

	configPath := testutil.ConfigFile(b, "upload-mitm.yaml", fmt.Sprintf(`
servers:
  - domain: %s
    routes:
      - prefix: /
        upstream: https://%s:%s
nameservers:
  - %s
`, benchClientDomain, benchUpstreamHost, port, stub.Address()))

	proxyInstance, err := New(configPath, NewTransport(nameservers.DialContext), tlsConfig, authority, nameservers)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	proxyURL := startListener(b, proxyInstance)

	return &mitmUploadHarness{
		mitmClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyURL(testutil.MustURL(proxyURL)),
				MaxIdleConns:        256,
				MaxIdleConnsPerHost: 64,
				TLSClientConfig:     &tls.Config{RootCAs: benchPool(caCert)},
			},
		},
		directClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        256,
				MaxIdleConnsPerHost: 64,
				TLSClientConfig:     &tls.Config{RootCAs: benchPool(upstream.Certificate())},
			},
		},
		tunnelClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyURL(testutil.MustURL(proxyURL)),
				MaxIdleConns:        256,
				MaxIdleConnsPerHost: 64,
				TLSClientConfig:     &tls.Config{RootCAs: benchPool(upstream.Certificate())},
			},
		},
		upstreamURL: upstream.URL,
		proxyTarget: "https://" + benchClientDomain + "/",
	}
}

// BenchmarkUploadMITM 稳态 MITM 上行吞吐：对照直连 HTTPS 上游，量化双重 TLS 加解密与代理转发对上传的叠加开销。
func BenchmarkUploadMITM(b *testing.B) {
	harness := newUploadMITMHarness(b)
	// 预热签发证书与 TLS 会话，避免首样本计入冷启动成本
	warm := bytes.Repeat([]byte("x"), 1<<10)
	if err := postDrain(harness.mitmClient, harness.proxyTarget, warm); err != nil {
		b.Fatalf("mitm warmup: %v", err)
	}
	if err := postDrain(harness.directClient, harness.upstreamURL, warm); err != nil {
		b.Fatalf("direct warmup: %v", err)
	}
	sizes := []struct {
		name string
		size int
	}{
		{"1MB", 1 << 20},
		{"16MB", 16 << 20},
	}
	for _, entry := range sizes {
		size := entry.size
		b.Run(entry.name+"/direct", func(b *testing.B) {
			benchUploadDirect(b, harness.directClient, harness.upstreamURL, size)
		})
		b.Run(entry.name+"/mitm", func(b *testing.B) {
			benchUploadProxy(b, harness.mitmClient, harness.proxyTarget, size)
		})
	}
}

// BenchmarkUploadMITMParallel 并发 MITM 上行吞吐：固定 1MB 请求体，按并发度分档，对照直连 HTTPS。
func BenchmarkUploadMITMParallel(b *testing.B) {
	harness := newUploadMITMHarness(b)
	warm := bytes.Repeat([]byte("x"), 1<<10)
	if err := postDrain(harness.mitmClient, harness.proxyTarget, warm); err != nil {
		b.Fatalf("mitm warmup: %v", err)
	}
	if err := postDrain(harness.directClient, harness.upstreamURL, warm); err != nil {
		b.Fatalf("direct warmup: %v", err)
	}
	const size = 1 << 20
	for _, parallelism := range []int{1, 4, 16} {
		label := fmt.Sprintf("p%d", parallelism)
		b.Run(label+"/direct", func(b *testing.B) {
			benchUploadParallelDirect(b, harness.directClient, harness.upstreamURL, size, parallelism)
		})
		b.Run(label+"/mitm", func(b *testing.B) {
			benchUploadParallelProxy(b, harness.mitmClient, harness.proxyTarget, size, parallelism)
		})
	}
}

// BenchmarkUploadModes 五模式上行吞吐对照：直连 HTTP、HTTP 代理、直连 HTTPS、HTTPS+隧道、HTTPS+MITM，同一体积并排比较，量化每层转发与加密的叠加开销。
func BenchmarkUploadModes(b *testing.B) {
	httpHarness := newUploadHarness(b)
	mitmHarness := newUploadMITMHarness(b)
	// 预热 HTTPS 路径（签发证书、双端 TLS 会话、CONNECT 隧道），避免冷启动混入稳态样本
	warm := bytes.Repeat([]byte("x"), 1<<10)
	for _, target := range []struct {
		client *http.Client
		url    string
	}{
		{mitmHarness.directClient, mitmHarness.upstreamURL},
		{mitmHarness.tunnelClient, mitmHarness.upstreamURL},
		{mitmHarness.mitmClient, mitmHarness.proxyTarget},
	} {
		if err := postDrain(target.client, target.url, warm); err != nil {
			b.Fatalf("warmup %s: %v", target.url, err)
		}
	}
	sizes := []struct {
		name string
		size int
	}{
		{"1MB", 1 << 20},
		{"16MB", 16 << 20},
	}
	for _, entry := range sizes {
		size := entry.size
		b.Run(entry.name+"/http-direct", func(b *testing.B) {
			benchUploadDirect(b, httpHarness.directClient, httpHarness.directURL, size)
		})
		b.Run(entry.name+"/http-proxy", func(b *testing.B) {
			benchUploadProxy(b, httpHarness.proxyClient, httpHarness.proxyTarget, size)
		})
		b.Run(entry.name+"/https-direct", func(b *testing.B) {
			benchUploadDirect(b, mitmHarness.directClient, mitmHarness.upstreamURL, size)
		})
		b.Run(entry.name+"/https-tunnel", func(b *testing.B) {
			benchUploadProxy(b, mitmHarness.tunnelClient, mitmHarness.upstreamURL, size)
		})
		b.Run(entry.name+"/https-mitm", func(b *testing.B) {
			benchUploadProxy(b, mitmHarness.mitmClient, mitmHarness.proxyTarget, size)
		})
	}
}
