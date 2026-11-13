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
	"strings"
	"testing"
	"time"

	"mrp/internal/ca"
	"mrp/internal/testutil"
)

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

// CONNECT 未命中路由且 MITM 开启时只走透传隧道：拨号失败仅回 502，不得再落入 MITM 分支补写 200 或启动 TLS 服务。
func TestConnect_TunnelDoesNotFallThroughToMITM(t *testing.T) {
	caCert, caKey := testutil.AuthorityCA(t)
	tlsConfig := &tls.Config{
		GetCertificate: ca.New(caCert, caKey).GetCertificate,
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: \"*.example.org\"\n    routes:\n      - prefix: /\n        upstream: http://unused\n")
	proxyURL, _ := startProxy(t, configPath, tlsConfig)
	addr := strings.TrimPrefix(proxyURL, "http://")

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	target := "a.b.example.org:443" // 多层子域未命中 *.example.org，走透传隧道，拨号失败
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	reader := bufio.NewReader(conn)
	if code := readConnectResponse(t, reader); code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", code)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if next, err := reader.ReadByte(); err == nil {
		t.Fatalf("tunnel failure must not fall through to MITM: got extra byte %q", next)
	}
}

// CONNECT 目标缺端口时隧道必须按补齐后的 host:port 拨号（老式 CONNECT/1.0 客户端），从而暴露把原始 Host 直接交给拨号链路的问题。在 443 上起回显服务验证可达性，特权端口不可绑定的环境跳过。
func TestConnect_NoPortTargetDialsDefaultPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:443")
	if err != nil {
		t.Skipf("bind 127.0.0.1:443 unavailable: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) { io.Copy(conn, conn); conn.Close() }(conn)
		}
	}()

	configPath := testutil.ConfigFile(t, "r.yaml", "servers: []\n")
	proxyURL, _ := startProxy(t, configPath, nil)
	addr := strings.TrimPrefix(proxyURL, "http://")

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	// CONNECT 与数据一次写入，回显到达即证明隧道按 127.0.0.1:443 建立成功
	fmt.Fprintf(conn, "CONNECT 127.0.0.1 HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\nPING\n")
	reader := bufio.NewReader(conn)
	if code := readConnectResponse(t, reader); code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
	line, err := reader.ReadString('\n')
	if err != nil || line != "PING\n" {
		t.Fatalf("tunnel should reach 127.0.0.1:443: got %q err=%v", line, err)
	}
}

func TestConnect_TunnelBadGateway(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {}))
	closed.Close()
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: placeholder.example.com\n    routes:\n      - prefix: /\n        upstream: http://127.0.0.1:1\n")
	proxyURL, _ := startProxy(t, configPath, nil)

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	target := strings.TrimPrefix(closed.URL, "http://")
	requestLine := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"
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

func TestConnect_PipelinedDataPreserved(t *testing.T) {
	echo := testutil.EchoServer(t)
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: placeholder.example.com\n    routes:\n      - prefix: /\n        upstream: http://127.0.0.1:1\n")
	proxyURL, _ := startProxy(t, configPath, nil)
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

// TestMITMConfig_FallsBackToConnectTarget 验证每连接 TLS 配置的回退逻辑：SNI 缺失（IP 直连客户端惯例）以 CONNECT 目标回退签发 IP SAN 证书，SNI 存在按 SNI 签发，静态证书或无 CA 的构造原样返回。
func TestMITMConfig_FallsBackToConnectTarget(t *testing.T) {
	caCert, caKey := testutil.AuthorityCA(t)
	authority := ca.New(caCert, caKey)
	template := &tls.Config{GetCertificate: authority.GetCertificate}

	cert, err := mitmTLSConfig(template, authority, "192.168.1.50").GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("fallback without SNI: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "192.168.1.50" {
		t.Fatalf("回退签发应含 IP SAN 192.168.1.50, got %v", leaf.IPAddresses)
	}

	sniCert, err := mitmTLSConfig(template, authority, "192.168.1.50").GetCertificate(&tls.ClientHelloInfo{ServerName: "api.example.com"})
	if err != nil {
		t.Fatalf("with SNI: %v", err)
	}
	leaf, err = x509.ParseCertificate(sniCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "api.example.com" {
		t.Fatalf("SNI 存在时应按 SNI 签发, got %v", leaf.DNSNames)
	}

	// 静态证书或无 CA 配置时原样返回，避免破坏无按名字签发能力的链路
	static := &tls.Config{Certificates: []tls.Certificate{testutil.SelfSignedCert(t, []string{"x"})}}
	if got := mitmTLSConfig(static, nil, "127.0.0.1"); got != static {
		t.Fatal("无 GetCertificate 的配置应原样返回")
	}
}

// 配置 domain 为 IP 的 HTTPS 路由：CONNECT 命中后 MITM 按 CONNECT 目标签发 IP SAN 证书（客户端对 IP 直连不发 SNI），客户端按 IP 校验通过，内层请求照常路由转发（改动前该场景必 502）。
func TestConnect_IPDomainMITM(t *testing.T) {
	upstream := recordingServer(t, "ip")
	caCert, caKey := testutil.AuthorityCA(t)
	authority := ca.New(caCert, caKey)
	tlsConfig := &tls.Config{
		GetCertificate: authority.GetCertificate,
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: 127.0.0.1\n    routes:\n      - prefix: /\n        upstream: "+upstream.URL+"\n        host: 127.0.0.1\n")
	// 直接构造并注入 authority：startProxy 基建固定传 nil，无法覆盖回退签发路径
	servers := testNameservers(t)
	proxy, err := New(configPath, NewTransport(servers.DialContext), tlsConfig, authority, servers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyURL := startListener(t, proxy)
	addr := strings.TrimPrefix(proxyURL, "http://")

	proxyConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer proxyConn.Close()
	fmt.Fprintf(proxyConn, "CONNECT 127.0.0.1:443 HTTP/1.1\r\nHost: 127.0.0.1:443\r\n\r\n")
	if code := readConnectResponse(t, bufio.NewReader(proxyConn)); code != 200 {
		t.Fatalf("CONNECT: want 200, got %d", code)
	}
	// Go 客户端对 IP 型 ServerName 不发 SNI，证书须由 CONNECT 目标回退签发，客户端按 IP 校验证书的 IPAddresses
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	innerConn := tls.Client(proxyConn, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1"})
	if err := innerConn.HandshakeContext(t.Context()); err != nil {
		t.Fatalf("inner handshake: %v", err)
	}
	fmt.Fprintf(innerConn, "GET /ip-path HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(innerConn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read inner response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimSpace(string(body)); got != "ip:/ip-path|host=127.0.0.1" {
		t.Fatalf("inner request: got %q", got)
	}
}

// 配置 domain 为 * 的 HTTPS 路由：任意域名的 CONNECT 均走 MITM 并按 CONNECT 目标域名签发证书，内层请求照常路由转发
func TestConnect_WildcardDomainMITM(t *testing.T) {
	upstream := recordingServer(t, "wild")
	caCert, caKey := testutil.AuthorityCA(t)
	authority := ca.New(caCert, caKey)
	tlsConfig := &tls.Config{
		GetCertificate: authority.GetCertificate,
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: \"*\"\n    routes:\n      - prefix: /\n        upstream: "+upstream.URL+"\n")
	servers := testNameservers(t)
	proxy, err := New(configPath, NewTransport(servers.DialContext), tlsConfig, authority, servers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyURL := startListener(t, proxy)
	addr := strings.TrimPrefix(proxyURL, "http://")

	proxyConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer proxyConn.Close()
	fmt.Fprintf(proxyConn, "CONNECT test.example.org:443 HTTP/1.1\r\nHost: test.example.org:443\r\n\r\n")
	if code := readConnectResponse(t, bufio.NewReader(proxyConn)); code != 200 {
		t.Fatalf("CONNECT: want 200, got %d", code)
	}
	// 客户端按 CA 校验证书链与目标域名，握手通过即证明 MITM 按 CONNECT 目标签发证书
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	innerConn := tls.Client(proxyConn, &tls.Config{RootCAs: roots, ServerName: "test.example.org"})
	if err := innerConn.HandshakeContext(t.Context()); err != nil {
		t.Fatalf("inner handshake with wildcard MITM certificate: %v", err)
	}
	fmt.Fprintf(innerConn, "GET /wild-path HTTP/1.1\r\nHost: test.example.org\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(innerConn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read inner response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimSpace(string(body)); !strings.HasPrefix(got, "wild:/wild-path") {
		t.Fatalf("inner request: got %q", got)
	}
}

// 配置 domain 为 *.suffix：单层子域 CONNECT 走 MITM，多层子域未命中走透传隧道（拨号失败返回 502）
func TestConnect_WildcardSuffixTunnelBoundary(t *testing.T) {
	caCert, caKey := testutil.AuthorityCA(t)
	tlsConfig := &tls.Config{
		GetCertificate: ca.New(caCert, caKey).GetCertificate,
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: \"*.example.org\"\n    routes:\n      - prefix: /\n        upstream: http://unused\n")
	proxyURL, _ := startProxy(t, configPath, tlsConfig)
	addr := strings.TrimPrefix(proxyURL, "http://")

	connect := func(target string) (net.Conn, int) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		fmt.Fprintf(conn, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", target, target)
		return conn, readConnectResponse(t, bufio.NewReader(conn))
	}
	handshake := func(conn net.Conn, serverName string) error {
		roots := x509.NewCertPool()
		roots.AddCert(caCert)
		innerConn := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: serverName})
		return innerConn.HandshakeContext(t.Context())
	}

	conn, code := connect("api.example.org")
	if code != 200 {
		t.Fatalf("single-label CONNECT: want 200, got %d", code)
	}
	if err := handshake(conn, "api.example.org"); err != nil {
		t.Fatalf("single-label should MITM with issued certificate: %v", err)
	}
	_, code = connect("a.b.example.org")
	if code != 502 {
		t.Fatalf("multi-label CONNECT should tunnel out and fail dialing: want 502, got %d", code)
	}
}

// CONNECT+MITM 后内部再发 CONNECT：验证嵌套 hijack 不被外层 defer 误关
func TestConnect_NestedConnectViaMITM(t *testing.T) {
	echo := testutil.EchoServer(t)
	caCert, caKey := testutil.AuthorityCA(t)
	tlsConfig := &tls.Config{
		GetCertificate: ca.New(caCert, caKey).GetCertificate,
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: http://unused\n")
	proxyURL, _ := startProxy(t, configPath, tlsConfig)
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
