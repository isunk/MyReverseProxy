package dns

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/isunk/MyReverseProxy/internal/cache"
	"github.com/isunk/MyReverseProxy/internal/logging"
)

const (
	nameserverDefaultPort = "53"
	dialerTimeout         = 5 * time.Second
	KeepAlive             = 30 * time.Second
	AttemptTimeout        = time.Second
	TTL                   = 30 * time.Second
	maxCached             = 512
)

var DefaultNameservers = []string{"114.114.114.114", "8.8.8.8"}

// Resolver 持有可热更新的 DNS 服务器列表，经 net.Resolver 注入拨号链路，
// 忽略设备 /etc/resolv.conf 里的服务器地址；解析结果按 TTL 缓存，
// 避免每条新建连接重复发 DNS 查询。
type Resolver struct {
	current        atomic.Pointer[state]
	attemptTimeout time.Duration
	ttl            time.Duration
	udpDialer      *net.Dialer
	tcpDialer      *net.Dialer
	cache          *cache.Cache[[]string]
}

type state struct {
	addresses []string
	resolvers []*net.Resolver
}

func New(attemptTimeout, ttl time.Duration, entries []string) (*Resolver, error) {
	addresses, err := normalizeNameservers(entries)
	if err != nil {
		return nil, err
	}
	set := &Resolver{
		attemptTimeout: attemptTimeout,
		ttl:            ttl,
		udpDialer:      &net.Dialer{Timeout: dialerTimeout},
		tcpDialer:      &net.Dialer{Timeout: dialerTimeout, KeepAlive: KeepAlive},
		cache:          cache.New[[]string](maxCached, ttl),
	}
	set.current.Store(&state{addresses: addresses, resolvers: set.buildResolvers(addresses)})
	return set, nil
}

// Update 用新的服务器列表重建解析器，列表未变化时直接返回。
func (r *Resolver) Update(entries []string) error {
	addresses, err := normalizeNameservers(entries)
	if err != nil {
		return err
	}
	current := r.current.Load()
	if slices.Equal(current.addresses, addresses) {
		return nil
	}
	r.current.Store(&state{addresses: addresses, resolvers: r.buildResolvers(addresses)})
	r.cache.Clear()
	logging.Infof("dns nameservers=%s", strings.Join(addresses, ","))
	return nil
}

// Addresses 返回当前生效的服务器地址列表，用于判断列表是否真正变化。
func (r *Resolver) Addresses() []string {
	return r.current.Load().addresses
}

// DialContext 按主机:端口取缓存的解析结果，未命中才发 DNS 查询，再逐个尝试解析出的地址。
func (r *Resolver) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(host) != nil {
		return r.connect(ctx, network, address)
	}
	return r.dialHost(ctx, network, host, port)
}

// dialHost 先取缓存的解析结果，未命中才发 DNS 查询，再逐个尝试解析出的地址。
func (r *Resolver) dialHost(ctx context.Context, network, host, port string) (net.Conn, error) {
	targets, fromCache, err := r.lookup(ctx, host, port)
	if err != nil {
		return nil, err
	}
	conn, lastError := r.connectAny(ctx, network, targets)
	if conn != nil || !fromCache {
		return conn, lastError
	}
	// 缓存的地址已连不上：视为解析结果过期，丢弃后立即重解析一次
	r.dropCached(host, port)
	fresh, _, err := r.lookup(ctx, host, port)
	if err != nil {
		return nil, err
	}
	return r.connectAny(ctx, network, fresh)
}

func (r *Resolver) connectAny(ctx context.Context, network string, targets []string) (net.Conn, error) {
	var lastError error
	for _, target := range targets {
		conn, err := r.connect(ctx, network, target)
		if err == nil {
			return conn, nil
		}
		lastError = err
	}
	return nil, lastError
}

func (r *Resolver) connect(ctx context.Context, network, address string) (net.Conn, error) {
	return r.tcpDialer.DialContext(ctx, network, address)
}

// lookup 命中未过期缓存即返回；否则向配置的 DNS 服务器查询并写入缓存。
// 第二个返回值标记本次结果来自缓存，调用方据此决定是否重试解析。
func (r *Resolver) lookup(ctx context.Context, host, port string) ([]string, bool, error) {
	key := net.JoinHostPort(host, port)
	if targets, ok := r.cache.Get(key); ok {
		return targets, true, nil
	}
	targets, err := r.resolve(ctx, host, port)
	if err != nil {
		return nil, false, err
	}
	r.cache.Put(key, targets)
	return targets, false, nil
}

// dropCached 丢弃缓存的解析结果，缓存地址全部连不上时调用以触发立即重解析。
func (r *Resolver) dropCached(host, port string) {
	r.cache.Evict(net.JoinHostPort(host, port))
}

// resolve 按配置顺序尝试 DNS 服务器，单次尝试超时后切换下一台。
func (r *Resolver) resolve(ctx context.Context, host, port string) ([]string, error) {
	var lastError error
	for _, resolver := range r.current.Load().resolvers {
		attempt, cancel := context.WithTimeout(ctx, r.attemptTimeout)
		ips, err := resolver.LookupIPAddr(attempt, host)
		cancel()
		if err == nil && len(ips) > 0 {
			return formatTargets(ips, port), nil
		}
		lastError = err
	}
	return nil, lastError
}

func formatTargets(ips []net.IPAddr, port string) []string {
	// 与 net.Dialer 对双栈网络的排序一致：IPv4 在前，避免无 IPv6 路由时每轮先失败一次
	slices.SortStableFunc(ips, func(a, b net.IPAddr) int {
		if (a.IP.To4() != nil) == (b.IP.To4() != nil) {
			return 0
		}
		if a.IP.To4() != nil {
			return -1
		}
		return 1
	})
	targets := make([]string, len(ips))
	for i, ip := range ips {
		targets[i] = net.JoinHostPort(ip.String(), port)
	}
	return targets
}

// ClearCache 清空解析缓存，配置变更后调用以丢弃旧解析结果。
func (r *Resolver) ClearCache() {
	r.cache.Clear()
}

// CacheSize 返回当前缓存的解析条目数，用于观测解析缓存是否生效。
func (r *Resolver) CacheSize() int {
	return r.cache.Size()
}

// buildResolvers 按地址构建解析器，每台 DNS 服务器一台，供 resolve 依次尝试。
func (r *Resolver) buildResolvers(addresses []string) []*net.Resolver {
	resolvers := make([]*net.Resolver, len(addresses))
	for i, address := range addresses {
		resolvers[i] = &net.Resolver{PreferGo: true, Dial: r.dialServer(address)}
	}
	return resolvers
}

// dialServer 固定拨向指定 DNS 服务器，忽略系统 resolv.conf 给出的服务器地址。
func (r *Resolver) dialServer(address string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return r.udpDialer.DialContext(ctx, network, address)
	}
}

func normalizeNameservers(entries []string) ([]string, error) {
	if len(entries) == 0 {
		entries = DefaultNameservers
	}
	addresses := make([]string, 0, len(entries))
	for _, entry := range entries {
		address, err := normalizeNameserver(entry)
		if err != nil {
			return nil, err
		}
		addresses = append(addresses, address)
	}
	return addresses, nil
}

func normalizeNameserver(entry string) (string, error) {
	host := strings.TrimSpace(entry)
	port := nameserverDefaultPort
	if parsedHost, parsedPort, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
		port = parsedPort
	}
	if ip := net.ParseIP(host); ip == nil {
		return "", fmt.Errorf("dns nameserver %q is not an IP literal", entry)
	}
	numericPort, err := strconv.Atoi(port)
	if err != nil || numericPort < 1 || numericPort > 65535 {
		return "", fmt.Errorf("dns nameserver %q has an invalid port", entry)
	}
	return net.JoinHostPort(host, port), nil
}
