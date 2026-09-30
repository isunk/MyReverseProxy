package proxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
