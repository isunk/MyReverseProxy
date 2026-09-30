package testutil

import (
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Stub 是应答固定记录的 DNS 测试替身：按查询类型返回预设地址，TTL 可按需指定。
type Stub struct {
	listener *net.UDPConn
	mu       sync.Mutex
	records  map[uint16][]net.IP
	ttl      uint16
	queries  atomic.Int32
}

// NewStub 启动应答预设记录的 DNS 服务器，TTL 为零。
func NewStub(t testing.TB, records map[uint16][]net.IP) *Stub {
	t.Helper()
	return NewStubWithTTL(t, records, 0)
}

// NewStubWithTTL 启动应答预设记录的 DNS 服务器，TTL 可控以验证缓存过期。
func NewStubWithTTL(t testing.TB, records map[uint16][]net.IP, ttl uint16) *Stub {
	t.Helper()
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	stub := &Stub{listener: listener.(*net.UDPConn), records: records, ttl: ttl}
	t.Cleanup(func() { _ = stub.listener.Close() })
	go stub.serve()
	return stub
}

func (s *Stub) SetRecords(records map[uint16][]net.IP) {
	s.mu.Lock()
	s.records = records
	s.mu.Unlock()
}

func (s *Stub) Address() string {
	return s.listener.LocalAddr().String()
}

func (s *Stub) QueryCount() int {
	return int(s.queries.Load())
}

func (s *Stub) serve() {
	buffer := make([]byte, 1500)
	for {
		size, remote, err := s.listener.ReadFrom(buffer)
		if err != nil {
			return
		}
		id, name, qtype, ok := parseDNSQuestion(buffer[:size])
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

// ClosedUDPPort 返回一个立即不可用的 UDP 端口，用于模拟已被拒绝的 DNS 服务器。
func ClosedUDPPort(t testing.TB) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()
	return strconv.Itoa(port)
}

// SilentUDPPort 返回一个只收包、永不回复的 UDP 端口，用于模拟超时的 DNS 服务器。
func SilentUDPPort(t testing.TB) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return strconv.Itoa(conn.LocalAddr().(*net.UDPAddr).Port)
}
