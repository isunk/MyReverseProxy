package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	nameserverDefaultPort    = "53"
	nameserverAttemptTimeout = 2 * time.Second
)

var defaultNameservers = []string{"114.114.114.114", "8.8.8.8"}

// nameserver 绑定固定出口地址的解析器，出口地址由 Dial 决定，忽略系统 resolv.conf 中的服务器列表。
type nameserver struct {
	address  string
	resolver *net.Resolver
}

// dnsResolver 串行遍历多个 nameserver 做故障切换，供 HTTP 上游与 CONNECT 隧道共用。
type dnsResolver struct {
	mu      sync.RWMutex
	servers []nameserver
	dialer  *net.Dialer
}

func newDNSResolver(dialTimeout time.Duration) (*dnsResolver, error) {
	resolver := &dnsResolver{dialer: &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}}
	if err := resolver.update(defaultNameservers); err != nil {
		return nil, err
	}
	return resolver, nil
}

func (r *dnsResolver) update(entries []string) error {
	next, err := buildNameservers(entries, r.dialer)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if nameserversEqual(r.servers, next) {
		return nil
	}
	r.servers = next
	logInfof("dns nameservers=%s", strings.Join(nameserverAddresses(next), ","))
	return nil
}

func (r *dnsResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	servers, err := r.snapshot()
	if err != nil {
		return nil, err
	}
	var failures []error
	for _, server := range servers {
		attemptCtx, cancel := attemptContext(ctx)
		addresses, err := server.resolver.LookupIPAddr(attemptCtx, host)
		cancel()
		if err == nil {
			return addresses, nil
		}
		failures = append(failures, err)
	}
	return nil, fmt.Errorf("dns lookup %s: %w", host, errors.Join(failures...))
}

func (r *dnsResolver) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("dial tcp %s: %v", address, err)
	}
	resolved, err := r.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("dial tcp %s: %v", address, err)
	}
	var lastError error
	for _, entry := range resolved {
		conn, err := r.dialer.DialContext(ctx, network, net.JoinHostPort(entry.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastError = err
	}
	return nil, fmt.Errorf("dial tcp %s: %v", address, lastError)
}

func (r *dnsResolver) snapshot() ([]nameserver, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.servers) == 0 {
		return nil, errors.New("dns has no nameserver")
	}
	snapshot := make([]nameserver, len(r.servers))
	copy(snapshot, r.servers)
	return snapshot, nil
}

func attemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, nameserverAttemptTimeout)
}

func buildNameservers(entries []string, dialer *net.Dialer) ([]nameserver, error) {
	addresses, err := normalizeNameservers(entries)
	if err != nil {
		return nil, err
	}
	servers := make([]nameserver, 0, len(addresses))
	for _, address := range addresses {
		servers = append(servers, newNameserver(address, dialer))
	}
	return servers, nil
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

func newNameserver(address string, dialer *net.Dialer) nameserver {
	return nameserver{
		address: address,
		resolver: &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, address)
			},
		},
	}
}

func nameserversEqual(first, second []nameserver) bool {
	if len(first) != len(second) {
		return false
	}
	for i, server := range first {
		if server.address != second[i].address {
			return false
		}
	}
	return true
}

func nameserverAddresses(servers []nameserver) []string {
	addresses := make([]string, len(servers))
	for i, server := range servers {
		addresses[i] = server.address
	}
	return addresses
}
