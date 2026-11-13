package proxy

import (
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"mrp/internal/testutil"
)

func TestParseSocks5UDPHeader(t *testing.T) {
	ipv4 := []byte{0x00, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0x1A, 0xE1, 'h', 'i'}
	header, ok := parseSocks5UDPHeader(ipv4)
	if !ok || header.host != "127.0.0.1" || header.port != 6881 || string(header.data) != "hi" || header.frag != 0 {
		t.Fatalf("ipv4 = %+v ok=%v, want 127.0.0.1:6881 data=hi", header, ok)
	}

	domain := []byte{0x00, 0x00, 0x00, 0x03, 0x03, 'a', 'p', 'i', 0x00, 0x50, 'x', 'y'}
	header, ok = parseSocks5UDPHeader(domain)
	if !ok || header.host != "api" || header.port != 80 || string(header.data) != "xy" {
		t.Fatalf("domain = %+v ok=%v, want api:80 data=xy", header, ok)
	}

	ipv6 := []byte{0x00, 0x00, 0x00, 0x04,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0x00, 0x50, 'z'}
	header, ok = parseSocks5UDPHeader(ipv6)
	if !ok || header.host != "::1" || header.port != 80 || string(header.data) != "z" {
		t.Fatalf("ipv6 = %+v ok=%v, want ::1:80 data=z", header, ok)
	}

	frag := []byte{0x00, 0x00, 0x01, 0x01, 127, 0, 0, 1, 0, 80, 'a'}
	header, ok = parseSocks5UDPHeader(frag)
	if !ok || header.frag != 1 {
		t.Fatalf("frag = %+v ok=%v, want frag=1", header, ok)
	}

	badRSV := []byte{0x01, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 80}
	if _, ok := parseSocks5UDPHeader(badRSV); ok {
		t.Fatal("bad RSV accepted")
	}

	badAtyp := []byte{0x00, 0x00, 0x00, 0x05, 0, 0, 0, 0, 0, 0}
	if _, ok := parseSocks5UDPHeader(badAtyp); ok {
		t.Fatal("bad atyp accepted")
	}

	if _, ok := parseSocks5UDPHeader([]byte{0x00, 0x00}); ok {
		t.Fatal("truncated accepted")
	}
}

func TestEncodeSocks5UDPResponse(t *testing.T) {
	addr := []byte{127, 0, 0, 1}
	encoded := encodeSocks5UDPResponse(socks5AtypIPv4, addr, 6881, []byte("hi"))
	header, ok := parseSocks5UDPHeader(encoded)
	if !ok || header.host != "127.0.0.1" || header.port != 6881 || string(header.data) != "hi" {
		t.Fatalf("round trip = %+v ok=%v, want 127.0.0.1:6881 data=hi", header, ok)
	}

	domain := []byte("api")
	encoded = encodeSocks5UDPResponse(socks5AtypDomain, domain, 80, []byte("xy"))
	header, ok = parseSocks5UDPHeader(encoded)
	if !ok || header.host != "api" || header.port != 80 || string(header.data) != "xy" {
		t.Fatalf("domain round trip = %+v ok=%v, want api:80 data=xy", header, ok)
	}
}

// socks5UDPAssociate 建立 SOCKS5 UDP ASSOCIATE，返回中继 UDP 地址与 TCP 控制连接。
func socks5UDPAssociate(t *testing.T, proxyAddr string) (net.Conn, string) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
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
	request := []byte{0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("write associate: %v", err)
	}
	reply = make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read associate reply: %v", err)
	}
	if reply[1] != socks5RepSuccess {
		t.Fatalf("associate reply rep = %d, want success", reply[1])
	}
	return conn, net.JoinHostPort(net.IP(reply[4:8]).String(), strconv.Itoa(int(reply[8])<<8|int(reply[9])))
}

func TestSocks5_UDPAssociateRelays(t *testing.T) {
	echoAddr := testutil.UDPEchoServer(t)
	servers := testNameservers(t)
	proxy, err := New(testutil.ConfigFile(t, "r.yaml", "servers: []\n"), NewTransport(servers.DialContext), nil, nil, servers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyURL := startListener(t, proxy)
	addr := strings.TrimPrefix(proxyURL, "http://")

	tcpConn, relayAddr := socks5UDPAssociate(t, addr)
	defer tcpConn.Close()

	echoHost, echoPort, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("split echo addr: %v", err)
	}
	relayConn, err := net.Dial("udp", relayAddr)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer relayConn.Close()

	// 构造 SOCKS5 UDP 报文头：IPv4 目标 + 负载。
	port, err := strconv.Atoi(echoPort)
	if err != nil {
		t.Fatalf("atoi echo port: %v", err)
	}
	header := []byte{0x00, 0x00, 0x00, 0x01}
	header = append(header, net.ParseIP(echoHost).To4()...)
	header = append(header, byte(port>>8), byte(port))
	payload := []byte("hello over socks5 udp")
	packet := append(header, payload...)
	if _, err := relayConn.Write(packet); err != nil {
		t.Fatalf("write relay: %v", err)
	}
	_ = relayConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buffer := make([]byte, 64<<10)
	n, err := relayConn.Read(buffer)
	if err != nil {
		t.Fatalf("read relay: %v", err)
	}
	response, ok := parseSocks5UDPHeader(buffer[:n])
	if !ok || string(response.data) != string(payload) {
		t.Fatalf("response = %+v ok=%v, want payload %q", response, ok, payload)
	}
}

// TestSocks5_UDPAssociateResolvesDomainViaNameservers 验证 SOCKS5 UDP 域名目标经自定义 DNS（nameservers）解析，而非系统 DNS。
func TestSocks5_UDPAssociateResolvesDomainViaNameservers(t *testing.T) {
	echoAddr := testutil.UDPEchoServer(t)
	_, echoPort, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("split echo addr: %v", err)
	}
	stub := testutil.NewStub(t, map[uint16][]net.IP{typeA: {net.ParseIP("127.0.0.1")}})
	nameservers := testNameservers(t, stub.Address())
	content := "servers: []\nnameservers:\n  - \"" + stub.Address() + "\"\n"
	proxy, err := New(testutil.ConfigFile(t, "r.yaml", content), NewTransport(nameservers.DialContext), nil, nil, nameservers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxyURL := startListener(t, proxy)
	addr := strings.TrimPrefix(proxyURL, "http://")

	tcpConn, relayAddr := socks5UDPAssociate(t, addr)
	defer tcpConn.Close()

	relayConn, err := net.Dial("udp", relayAddr)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer relayConn.Close()

	port, err := strconv.Atoi(echoPort)
	if err != nil {
		t.Fatalf("atoi echo port: %v", err)
	}
	const domain = "udp-test.local"
	header := []byte{0x00, 0x00, 0x00, 0x03, byte(len(domain))}
	header = append(header, domain...)
	header = append(header, byte(port>>8), byte(port))
	payload := []byte("hello over socks5 udp domain")
	packet := append(header, payload...)
	if _, err := relayConn.Write(packet); err != nil {
		t.Fatalf("write relay: %v", err)
	}
	_ = relayConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buffer := make([]byte, 64<<10)
	n, err := relayConn.Read(buffer)
	if err != nil {
		t.Fatalf("read relay: %v", err)
	}
	response, ok := parseSocks5UDPHeader(buffer[:n])
	if !ok || string(response.data) != string(payload) {
		t.Fatalf("response = %+v ok=%v, want payload %q", response, ok, payload)
	}
}
