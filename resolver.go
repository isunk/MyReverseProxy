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
	nameserverDefaultPort    = "53"
	nameserverAttemptTimeout = 2 * time.Second
	dialKeepAlive            = 30 * time.Second
)

var defaultNameservers = []string{"114.114.114.114", "8.8.8.8"}

// nameserverSet 持有可热更新的 DNS 服务器列表，经 net.Dialer.Resolver 注入拨号链路，
// 忽略设备 /etc/resolv.conf 里的服务器地址。
type nameserverSet struct {
	current   atomic.Pointer[nameserverState]
	udpDialer *net.Dialer
}

type nameserverState struct {
	addresses []string
	dialers   []*net.Dialer
}

func newNameserverSet(dialTimeout time.Duration, entries []string) (*nameserverSet, error) {
	addresses, err := normalizeNameservers(entries)
	if err != nil {
		return nil, err
	}
	set := &nameserverSet{udpDialer: &net.Dialer{Timeout: dialTimeout}}
	set.current.Store(&nameserverState{addresses: addresses, dialers: buildDialers(set, addresses, dialTimeout)})
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
	dialTimeout := s.udpDialer.Timeout
	next := &nameserverState{addresses: addresses, dialers: buildDialers(s, addresses, dialTimeout)}
	s.current.Store(next)
	logInfof("dns nameservers=%s", strings.Join(addresses, ","))
	return nil
}

func (s *nameserverSet) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var lastError error
	for _, dialer := range s.current.Load().dialers {
		attempt, cancel := context.WithTimeout(ctx, nameserverAttemptTimeout)
		conn, err := dialer.DialContext(attempt, network, address)
		cancel()
		if err == nil {
			return conn, nil
		}
		lastError = err
	}
	return nil, lastError
}

func (s *nameserverSet) serverAddresses() []string {
	return s.current.Load().addresses
}

func buildDialers(set *nameserverSet, addresses []string, dialTimeout time.Duration) []*net.Dialer {
	dialers := make([]*net.Dialer, len(addresses))
	for i, address := range addresses {
		dialers[i] = &net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: dialKeepAlive,
			Resolver:  &net.Resolver{PreferGo: true, Dial: set.dialServer(address)},
		}
	}
	return dialers
}

func (s *nameserverSet) dialServer(address string) func(context.Context, string, string) (net.Conn, error) {
	// 忽略系统 resolv.conf 给出的服务器地址，固定拨向本 DNS 服务器。
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
