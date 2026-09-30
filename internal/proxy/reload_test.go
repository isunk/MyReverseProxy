package proxy

import (
	"context"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/isunk/MyReverseProxy/internal/testutil"
)

func TestReload_SwitchesRoute(t *testing.T) {
	upA := recordingServer(t, "A")
	upB := recordingServer(t, "B")
	configPath := testutil.ConfigFile(t, "r.yaml", "servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upA.URL+"\n")
	servers := testNameservers(t)
	proxy, err := New(configPath, NewTransport(servers.DialContext), nil, nil, servers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := proxyClient(startListener(t, proxy))

	if got := requestBody(t, client, "http://api.example.com/x"); !strings.HasPrefix(got, "A:/x") {
		t.Fatalf("before reload: %q", got)
	}
	_ = os.WriteFile(configPath, []byte("servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upB.URL+"\n"), 0o644)
	if err := proxy.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := requestBody(t, client, "http://api.example.com/x"); !strings.HasPrefix(got, "B:/x") {
		t.Fatalf("after reload: %q", got)
	}
}

func TestReload_UnchangedConfigKeepsIdleConnections(t *testing.T) {
	upA := recordingServer(t, "A")
	upB := recordingServer(t, "B")
	config := "servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: " + upA.URL + "\n"
	configPath := testutil.ConfigFile(t, "r.yaml", config)

	var dials int32
	servers := testNameservers(t)
	dialing := func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := servers.DialContext(ctx, network, address)
		if err == nil {
			atomic.AddInt32(&dials, 1)
		}
		return conn, err
	}
	proxy, err := New(configPath, NewTransport(dialing), nil, nil, servers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := proxyClient(startListener(t, proxy))

	getOnce(t, client, "http://api.example.com/one", "A:/one")
	if got := atomic.LoadInt32(&dials); got != 1 {
		t.Fatalf("dials after first request = %d, want 1", got)
	}
	if err := proxy.Reload(); err != nil {
		t.Fatalf("Reload unchanged config: %v", err)
	}
	getOnce(t, client, "http://api.example.com/two", "A:/two")
	if got := atomic.LoadInt32(&dials); got != 1 {
		t.Fatalf("dials after unchanged reload = %d, want 1 (idle upstream connection was torn down)", got)
	}
	// 真正变更仍需重建路由并拆掉指向旧目标的连接
	if err := os.WriteFile(configPath, []byte(strings.Replace(config, upA.URL, upB.URL, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := proxy.Reload(); err != nil {
		t.Fatalf("Reload changed config: %v", err)
	}
	getOnce(t, client, "http://api.example.com/three", "B:/three")
	if got := atomic.LoadInt32(&dials); got != 2 {
		t.Fatalf("dials after route change = %d, want 2", got)
	}
}

// 路由变更后清掉解析缓存；仅改注释不触发
func TestReload_ClearsDNSCache(t *testing.T) {
	up := recordingServer(t, "upstream")
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	stub := testutil.NewStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	config := "servers:\n" +
		"  - domain: api.example.com\n" +
		"    routes:\n" +
		"      - prefix: /\n" +
		"        upstream: http://api.mrp.local:" + port + "\n" +
		"nameservers:\n" +
		"  - \"" + stub.Address() + "\"\n"
	servers := testNameservers(t)
	configPath := testutil.ConfigFile(t, "r.yaml", config)
	proxy, err := New(configPath, NewTransport(servers.DialContext), nil, nil, servers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := proxyClient(startListener(t, proxy))

	getOnce(t, client, "http://api.example.com/one", "upstream:/one")
	if got := servers.CacheSize(); got != 1 {
		t.Fatalf("cached entries after first request = %d, want 1", got)
	}
	if got := stub.QueryCount(); got != 2 {
		t.Fatalf("queries after first request = %d, want 2", got)
	}
	// 仅改注释：不重建路由，也不清解析缓存
	if err := os.WriteFile(configPath, []byte("# comment only\n"+config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := proxy.Reload(); err != nil {
		t.Fatalf("Reload comment-only change: %v", err)
	}
	if got := servers.CacheSize(); got != 1 {
		t.Fatalf("cached entries after comment-only reload = %d, want 1", got)
	}
	// 改上游地址：必须丢弃旧解析结果
	if err := os.WriteFile(configPath, []byte(strings.Replace(config, "api.mrp.local", "api2.mrp.local", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := proxy.Reload(); err != nil {
		t.Fatalf("Reload upstream change: %v", err)
	}
	if got := servers.CacheSize(); got != 0 {
		t.Fatalf("cached entries after upstream change = %d, want 0", got)
	}
	getOnce(t, client, "http://api.example.com/two", "upstream:/two")
	if got := stub.QueryCount(); got != 4 {
		t.Fatalf("queries after upstream change = %d, want 4", got)
	}
}

func TestWatch_HotReload(t *testing.T) {
	upA := recordingServer(t, "A")
	upB := recordingServer(t, "B")
	configPath := testutil.ConfigFile(t, "r.yaml", "servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upA.URL+"\n")
	servers := testNameservers(t)
	proxy, err := New(configPath, NewTransport(servers.DialContext), nil, nil, servers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := proxyClient(startListener(t, proxy))

	stop := make(chan struct{})
	go proxy.WatchFile(20*time.Millisecond, stop)
	t.Cleanup(func() { close(stop) })

	if got := requestBody(t, client, "http://api.example.com/x"); !strings.HasPrefix(got, "A:/x") {
		t.Fatalf("before hot reload: %q", got)
	}
	_ = os.WriteFile(configPath, []byte("servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upB.URL+"\n"), 0o644)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if got := requestBody(t, client, "http://api.example.com/x"); strings.HasPrefix(got, "B:/x") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("配置变更未自动热加载")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
