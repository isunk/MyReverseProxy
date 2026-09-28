package main

import (
	"net/http/httputil"
	"net/url"
	"strings"
)

type route struct {
	prefix          string
	target          *url.URL
	host            string
	requestHeaders  map[string]string
	responseHeaders map[string]string
	proxy           *httputil.ReverseProxy
}

type routeTable struct {
	byDomain map[string][]*route
}

func (t *routeTable) has(domain string) bool {
	_, ok := t.byDomain[domain]
	return ok
}

func (t *routeTable) pick(domain, path string) (*route, bool) {
	var best *route
	for _, entry := range t.byDomain[domain] {
		if pathHasPrefix(path, entry.prefix) && (best == nil || len(entry.prefix) > len(best.prefix)) {
			best = entry
		}
	}
	return best, best != nil
}

// pathHasPrefix 按路径段边界匹配前缀：/api 匹配 /api 和 /api/x，不匹配 /api-v2
func pathHasPrefix(path, prefix string) bool {
	if prefix == "/" {
		return true
	}
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	if strings.HasSuffix(prefix, "/") {
		return true
	}
	return len(path) == len(prefix) || path[len(prefix)] == '/'
}
