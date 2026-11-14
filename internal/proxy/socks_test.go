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

func TestSocks5ReadCommand(t *testing.T) {
	read := func(data []byte) (byte, byte, byte) {
		return socks5ReadCommand(bufio.NewReader(bytes.NewReader(data)))
	}
	cases := []struct {
		name    string
		data    []byte
		command byte
		atyp    byte
		rep     byte
	}{
		{"connect_ipv4", []byte{0x05, 0x01, 0x00, 0x01}, socks5CommandConnect, socks5AtypIPv4, socks5RepSuccess},
		{"connect_domain", []byte{0x05, 0x01, 0x00, 0x03}, socks5CommandConnect, socks5AtypDomain, socks5RepSuccess},
		{"udp_associate", []byte{0x05, 0x03, 0x00, 0x01}, socks5CommandUDPAssociate, socks5AtypIPv4, socks5RepSuccess},
		{"bind_unsupported", []byte{0x05, 0x02, 0x00, 0x01}, 0, 0, socks5RepCommandUnsupported},
		{"bad_version", []byte{0x04, 0x01, 0x00, 0x01}, 0, 0, socks5RepGeneralFailure},
		{"bad_rsv", []byte{0x05, 0x01, 0x01, 0x01}, 0, 0, socks5RepGeneralFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			command, atyp, rep := read(tc.data)
			if command != tc.command || atyp != tc.atyp || rep != tc.rep {
				t.Fatalf("got command=%d atyp=%d rep=%d, want %d/%d/%d", command, atyp, rep, tc.command, tc.atyp, tc.rep)
			}
		})
	}
}

func TestSocks5ReadAddr(t *testing.T) {
	ipv4 := []byte{127, 0, 0, 1, 0x1A, 0xE1}
	host, port, rep := socks5ReadAddr(bufio.NewReader(bytes.NewReader(ipv4)), socks5AtypIPv4)
	if rep != socks5RepSuccess || host != "127.0.0.1" || port != 6881 {
		t.Fatalf("ipv4 = %q:%d rep=%d, want 127.0.0.1:6881 success", host, port, rep)
	}

	domain := []byte{0x03, 'a', 'p', 'i', 0x00, 0x50}
	host, port, rep = socks5ReadAddr(bufio.NewReader(bytes.NewReader(domain)), socks5AtypDomain)
	if rep != socks5RepSuccess || host != "api" || port != 80 {
		t.Fatalf("domain = %q:%d rep=%d, want api:80 success", host, port, rep)
	}

	ipv6 := []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0x00, 0x50}
	host, port, rep = socks5ReadAddr(bufio.NewReader(bytes.NewReader(ipv6)), socks5AtypIPv6)
	if rep != socks5RepSuccess || host != "::1" || port != 80 {
		t.Fatalf("ipv6 = %q:%d rep=%d, want ::1:80 success", host, port, rep)
	}

	_, _, rep = socks5ReadAddr(bufio.NewReader(bytes.NewReader([]byte{0x05, 0, 0, 0, 0, 0})), 0x05)
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

// TestSocks5_UDPAssociateBadAtypRejected 验证 UDP ASSOCIATE 请求带非法 ATYP 时写回 0x08（与 CONNECT 分支一致），而非静默进入中继。
func TestSocks5_UDPAssociateBadAtypRejected(t *testing.T) {
	servers := testNameservers(t)
	proxy, err := New(testutil.ConfigFile(t, "r.yaml", "servers: []\n"), NewTransport(servers.DialContext), nil, nil, servers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyURL := startListener(t, proxy)
	addr := strings.TrimPrefix(proxyURL, "http://")

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("write greeting: %v", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read greeting reply: %v", err)
	}
	request := []byte{0x05, 0x03, 0x00, 0x05} // UDP ASSOCIATE + 非法 ATYP 0x05
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("write associate: %v", err)
	}
	reply = make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read associate reply: %v", err)
	}
	if reply[1] != socks5RepAtypUnsupported {
		t.Fatalf("associate reply rep = %d, want %d (atyp unsupported)", reply[1], socks5RepAtypUnsupported)
	}
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
