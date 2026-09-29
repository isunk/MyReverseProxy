package main

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	nameserverDefaultPort = "53"
	dialerTimeout         = 5 * time.Second
	defaultAttemptTimeout = time.Second
	defaultDNSTTL         = 30 * time.Second
	dialKeepAlive         = 30 * time.Second
	maxCachedDNS          = 512
)

var defaultNameservers = []string{"114.114.114.114", "8.8.8.8"}

// nameserverSet 持有可热更新的 DNS 服务器列表，经 net.Resolver 注入拨号链路，
// 忽略设备 /etc/resolv.conf 里的服务器地址；解析结果按 dnsTTL 缓存，
// 避免每条新建连接重复发 DNS 查询。
type nameserverSet struct {
	current        atomic.Pointer[nameserverState]
	attemptTimeout time.Duration
	dnsTTL         time.Duration
	udpDialer      *net.Dialer
	tcpDialer      *net.Dialer
	cache          *expiringCache[[]string]
}

type nameserverState struct {
	addresses []string
	resolvers []*net.Resolver
}

func newNameserverSet(attemptTimeout, dnsTTL time.Duration, entries []string) (*nameserverSet, error) {
	addresses, err := normalizeNameservers(entries)
	if err != nil {
		return nil, err
	}
	set := &nameserverSet{
		attemptTimeout: attemptTimeout,
		dnsTTL:         dnsTTL,
		udpDialer:      &net.Dialer{Timeout: dialerTimeout},
		tcpDialer:      &net.Dialer{Timeout: dialerTimeout, KeepAlive: dialKeepAlive},
		cache:          newExpiringCache[[]string](maxCachedDNS, dnsTTL),
	}
	set.current.Store(&nameserverState{addresses: addresses, resolvers: set.buildResolvers(addresses)})
	return set, nil
}

func (s *nameserverSet) update(entries []string) error {
	addresses, err := normalizeNameservers(entries)
	if err != nil {
		return err
	}
	current := s.current.Load()
	if slices.Equal(current.addresses, addresses) {
		return nil
	}
	s.current.Store(&nameserverState{addresses: addresses, resolvers: s.buildResolvers(addresses)})
	s.cache.clear()
	logInfof("dns nameservers=%s", strings.Join(addresses, ","))
	return nil
}

func (s *nameserverSet) serverAddresses() []string {
	return s.current.Load().addresses
}

// DialContext 按主机:端口取缓存的解析结果，未命中才发 DNS 查询，再逐个尝试解析出的地址。
func (s *nameserverSet) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(host) != nil {
		return s.connect(ctx, network, address)
	}
	return s.dialHost(ctx, network, host, port)
}

// dialHost 先取缓存的解析结果，未命中才发 DNS 查询，再逐个尝试解析出的地址。
func (s *nameserverSet) dialHost(ctx context.Context, network, host, port string) (net.Conn, error) {
	targets, fromCache, err := s.lookup(ctx, host, port)
	if err != nil {
		return nil, err
	}
	conn, lastError := s.connectAny(ctx, network, targets)
	if conn != nil || !fromCache {
		return conn, lastError
	}
	// 缓存的地址已连不上：视为解析结果过期，丢弃后立即重解析一次
	s.dropCached(host, port)
	fresh, _, err := s.lookup(ctx, host, port)
	if err != nil {
		return nil, err
	}
	return s.connectAny(ctx, network, fresh)
}

func (s *nameserverSet) connectAny(ctx context.Context, network string, targets []string) (net.Conn, error) {
	var lastError error
	for _, target := range targets {
		conn, err := s.connect(ctx, network, target)
		if err == nil {
			return conn, nil
		}
		lastError = err
	}
	return nil, lastError
}

func (s *nameserverSet) connect(ctx context.Context, network, address string) (net.Conn, error) {
	return s.tcpDialer.DialContext(ctx, network, address)
}

// lookup 命中未过期缓存即返回；否则向配置的 DNS 服务器查询并写入缓存。
// 第二个返回值标记本次结果来自缓存，调用方据此决定是否重试解析。
func (s *nameserverSet) lookup(ctx context.Context, host, port string) ([]string, bool, error) {
	key := net.JoinHostPort(host, port)
	if targets, ok := s.cache.get(key); ok {
		return targets, true, nil
	}
	targets, err := s.resolve(ctx, host, port)
	if err != nil {
		return nil, false, err
	}
	s.cache.put(key, targets)
	return targets, false, nil
}

// dropCached 丢弃缓存的解析结果，缓存地址全部连不上时调用以触发立即重解析。
func (s *nameserverSet) dropCached(host, port string) {
	s.cache.evict(net.JoinHostPort(host, port))
}

// resolve 按配置顺序尝试 DNS 服务器，单次尝试超时后切换下一台。
func (s *nameserverSet) resolve(ctx context.Context, host, port string) ([]string, error) {
	var lastError error
	for _, resolver := range s.current.Load().resolvers {
		attempt, cancel := context.WithTimeout(ctx, s.attemptTimeout)
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

// clearCache 清空解析缓存，配置变更后调用以丢弃旧解析结果。
func (s *nameserverSet) clearCache() {
	s.cache.clear()
}

// buildResolvers 按地址构建解析器，每台 DNS 服务器一台，供 resolve 依次尝试。
func (s *nameserverSet) buildResolvers(addresses []string) []*net.Resolver {
	resolvers := make([]*net.Resolver, len(addresses))
	for i, address := range addresses {
		resolvers[i] = &net.Resolver{PreferGo: true, Dial: s.dialServer(address)}
	}
	return resolvers
}

// dialServer 固定拨向指定 DNS 服务器，忽略系统 resolv.conf 给出的服务器地址。
func (s *nameserverSet) dialServer(address string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return s.udpDialer.DialContext(ctx, network, address)
	}
}

func normalizeNameservers(entries []string) ([]string, error) {
	if len(entries) == 0 {
		entries = defaultNameservers
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
