package main

import (
	"net/http/httputil"
	"net/url"
	"strings"
)

type route struct {
	prefix string
	target *url.URL
	host   string
	proxy  *httputil.ReverseProxy
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
		if strings.HasPrefix(path, entry.prefix) && (best == nil || len(entry.prefix) > len(best.prefix)) {
			best = entry
		}
	}
	return best, best != nil
}
