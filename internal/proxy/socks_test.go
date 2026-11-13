package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"mrp/internal/testutil"
)

func TestSocks5ReadConnect(t *testing.T) {
	ipv4 := bytes.NewReader(append([]byte{0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1}, 0x1A, 0xE1))
	host, port, rep := socks5ReadConnect(bufio.NewReader(ipv4))
	if rep != socks5RepSuccess || host != "127.0.0.1" || port != 6881 {
		t.Fatalf("ipv4 = %q:%d rep=%d, want 127.0.0.1:6881 success", host, port, rep)
	}

	domain := append([]byte{0x05, 0x01, 0x00, 0x03, 0x03, 'a', 'p', 'i'}, 0x00, 0x50)
	host, port, rep = socks5ReadConnect(bufio.NewReader(bytes.NewReader(domain)))
	if rep != socks5RepSuccess || host != "api" || port != 80 {
		t.Fatalf("domain = %q:%d rep=%d, want api:80 success", host, port, rep)
	}

	ipv6 := append([]byte{0x05, 0x01, 0x00, 0x04,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, 0x00, 0x50)
	host, port, rep = socks5ReadConnect(bufio.NewReader(bytes.NewReader(ipv6)))
	if rep != socks5RepSuccess || host != "::1" || port != 80 {
		t.Fatalf("ipv6 = %q:%d rep=%d, want ::1:80 success", host, port, rep)
	}

	bind := []byte{0x05, 0x02, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	_, _, rep = socks5ReadConnect(bufio.NewReader(bytes.NewReader(bind)))
	if rep != socks5RepCommandUnsupported {
		t.Fatalf("bind rep = %d, want command unsupported", rep)
	}

	unknown := []byte{0x05, 0x01, 0x00, 0x05, 0, 0, 0, 0, 0, 0}
	_, _, rep = socks5ReadConnect(bufio.NewReader(bytes.NewReader(unknown)))
	if rep != socks5RepAtypUnsupported {
		t.Fatalf("unknown atyp rep = %d, want atyp unsupported", rep)
	}
}

// socks5Connect 手写 SOCKS5 无认证握手并 CONNECT 目标（域名 ATYP），返回进入透传阶段的连接。
func socks5Connect(t *testing.T, proxyAddr, target string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("write greeting: %v", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read greeting reply: %v", err)
	}
	if reply[0] != 0x05 || reply[1] != 0x00 {
		t.Fatalf("greeting reply = %v, want no-auth", reply)
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatalf("split target: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	request := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	request = append(request, host...)
	request = append(request, byte(port>>8), byte(port))
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("write connect: %v", err)
	}
	reply = make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read connect reply: %v", err)
	}
	if reply[1] != socks5RepSuccess {
		t.Fatalf("connect reply rep = %d, want success", reply[1])
	}
	return conn
}

func TestSocks5_ConnectRelaysTCP(t *testing.T) {
	echoAddr := testutil.EchoServer(t)
	servers := testNameservers(t)
	proxy, err := New(testutil.ConfigFile(t, "r.yaml", "servers: []\n"), NewTransport(servers.DialContext), nil, nil, servers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyURL := startListener(t, proxy)
	addr := strings.TrimPrefix(proxyURL, "http://")

	conn := socks5Connect(t, addr, echoAddr)
	defer conn.Close()
	payload := "hello over socks5"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != payload {
		t.Fatalf("echo = %q, want %q", buf, payload)
	}
}
