package dns

import (
	"bytes"
	"context"
	"io"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/isunk/MyReverseProxy/internal/logging"
	"github.com/isunk/MyReverseProxy/internal/testutil"
)

func init() {
	logging.SetOutput(io.Discard)
}

func testNameservers(t *testing.T, entries ...string) *Resolver {
	t.Helper()
	return testNameserversWithTimeout(t, AttemptTimeout, TTL, entries)
}

func testNameserversWithTimeout(t *testing.T, attemptTimeout, ttl time.Duration, entries []string) *Resolver {
	t.Helper()
	set, err := New(attemptTimeout, ttl, entries)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return set
}

func TestNormalizeNameserver(t *testing.T) {
	tests := []struct {
		input string
		want  string
		fail  bool
	}{
		{"114.114.114.114", "114.114.114.114:53", false},
		{"8.8.8.8:5353", "8.8.8.8:5353", false},
		{"[::1]:53", "[::1]:53", false},
		{"  1.1.1.1  ", "1.1.1.1:53", false},
		{"", "", true},
		{"dns.example.com", "", true},
		{"127.0.0.1:abc", "", true},
		{"127.0.0.1:0", "", true},
		{"127.0.0.1:65536", "", true},
	}
	for _, tc := range tests {
		got, err := normalizeNameserver(tc.input)
		if tc.fail {
			if err == nil {
				t.Fatalf("normalizeNameserver(%q) accepted %q", tc.input, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("normalizeNameserver(%q): %v", tc.input, err)
		}
		if got != tc.want {
			t.Fatalf("normalizeNameserver(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestNormalizeNameservers_Defaults(t *testing.T) {
	addresses, err := normalizeNameservers(nil)
	if err != nil {
		t.Fatalf("normalizeNameservers: %v", err)
	}
	want := []string{"114.114.114.114:53", "8.8.8.8:53"}
	if !slices.Equal(addresses, want) {
		t.Fatalf("defaults = %v, want %v", addresses, want)
	}
}

func TestResolver_Failover(t *testing.T) {
	echo := testutil.EchoServer(t)
	_, port, _ := net.SplitHostPort(echo)
	stub := testutil.NewStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	set := testNameservers(t)
	if err := set.Update([]string{"127.0.0.1:" + testutil.ClosedUDPPort(t), stub.Address()}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	conn, err := set.DialContext(context.Background(), "tcp", "echo.mrp.local:"+port)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	payload := []byte("ping")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(echoed, payload) {
		t.Fatalf("echo = %q, want %q", echoed, payload)
	}
}

// 首个 nameserver 只收包不回复，验证单次尝试超时会真正生效并限制故障切换等待
func TestResolver_AttemptTimeout(t *testing.T) {
	echo := testutil.EchoServer(t)
	_, port, _ := net.SplitHostPort(echo)
	stub := testutil.NewStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	set := testNameserversWithTimeout(t, 50*time.Millisecond, TTL,
		[]string{"127.0.0.1:" + testutil.SilentUDPPort(t), stub.Address()})
	started := time.Now()
	conn, err := set.DialContext(context.Background(), "tcp", "echo.mrp.local:"+port)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if elapsed < 40*time.Millisecond {
		t.Fatalf("failover took %v, first nameserver was not waited on", elapsed)
	}
	if elapsed > time.Second {
		t.Fatalf("failover took %v, per-nameserver attempt timeout not applied", elapsed)
	}
}

// 首次连接解析并缓存，同主机:端口的后续连接直接复用解析结果
func TestResolver_CachesResolution(t *testing.T) {
	echo := testutil.EchoServer(t)
	_, port, _ := net.SplitHostPort(echo)
	stub := testutil.NewStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	set := testNameservers(t, stub.Address())
	for i := range 3 {
		conn, err := set.DialContext(context.Background(), "tcp", "up.example.com:"+port)
		if err != nil {
			t.Fatalf("dial %d: %v", i+1, err)
		}
		conn.Close()
	}
	if got := stub.QueryCount(); got != 2 {
		t.Fatalf("dns queries for 3 dials = %d, want 2 (one lookup then cache hits)", got)
	}
}

func TestResolver_CacheTTLExpires(t *testing.T) {
	echo := testutil.EchoServer(t)
	_, port, _ := net.SplitHostPort(echo)
	stub := testutil.NewStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	set := testNameserversWithTimeout(t, AttemptTimeout, 20*time.Millisecond, []string{stub.Address()})
	dial := func() {
		conn, err := set.DialContext(context.Background(), "tcp", "up.example.com:"+port)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		conn.Close()
	}
	dial()
	if got := stub.QueryCount(); got != 2 {
		t.Fatalf("queries before expiry = %d, want 2", got)
	}
	time.Sleep(50 * time.Millisecond)
	dial()
	if got := stub.QueryCount(); got != 4 {
		t.Fatalf("queries after ttl expiry = %d, want 4", got)
	}
}

// TTL 为 0 时禁用缓存，每次连接都重新解析
func TestResolver_CacheDisabled(t *testing.T) {
	echo := testutil.EchoServer(t)
	_, port, _ := net.SplitHostPort(echo)
	stub := testutil.NewStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	set := testNameserversWithTimeout(t, AttemptTimeout, 0, []string{stub.Address()})
	for i := range 2 {
		conn, err := set.DialContext(context.Background(), "tcp", "up.example.com:"+port)
		if err != nil {
			t.Fatalf("dial %d: %v", i+1, err)
		}
		conn.Close()
	}
	if got := stub.QueryCount(); got != 4 {
		t.Fatalf("queries with cache disabled = %d, want 4", got)
	}
	if got := set.CacheSize(); got != 0 {
		t.Fatalf("cached entries with cache disabled = %d, want 0", got)
	}
}

// 缓存的地址连不上时立即重解析，不等 TTL 过期
func TestResolver_StaleCacheRetries(t *testing.T) {
	echo := testutil.EchoServer(t)
	_, port, _ := net.SplitHostPort(echo)
	stub := testutil.NewStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.2")}})
	set := testNameservers(t, stub.Address())
	if _, err := set.DialContext(context.Background(), "tcp", "up.example.com:"+port); err == nil {
		t.Fatal("dial to an unreachable address should fail")
	}
	if got := stub.QueryCount(); got != 2 {
		t.Fatalf("queries after first dial = %d, want 2", got)
	}
	stub.SetRecords(map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	conn, err := set.DialContext(context.Background(), "tcp", "up.example.com:"+port)
	if err != nil {
		t.Fatalf("dial after upstream moved: %v", err)
	}
	payload := []byte("ping")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = conn.Close()
	if !bytes.Equal(echoed, payload) {
		t.Fatalf("echo = %q, want %q", echoed, payload)
	}
	if got := stub.QueryCount(); got != 4 {
		t.Fatalf("stale cached address was not evicted and re-resolved, queries = %d", got)
	}
}

func TestResolver_UpdateFailureKeepsServers(t *testing.T) {
	stub := testutil.NewStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	set := testNameservers(t, stub.Address())
	if err := set.Update([]string{"dns.example.com"}); err == nil {
		t.Fatal("hostname nameserver should be rejected")
	}
	if got := set.Addresses(); !slices.Equal(got, []string{stub.Address()}) {
		t.Fatalf("nameservers after failed update = %v, want %v", got, []string{stub.Address()})
	}
}
