package main

import (
	"net/http/httputil"
	"net/url"
	"strings"
)

type route_entry struct {
	prefix        string
	target        *url.URL
	host          string
	skip_verify   bool
	reverse_proxy *httputil.ReverseProxy
}

type route_table struct {
	by_domain map[string][]*route_entry
}

func (t *route_table) pick(domain, path string) (*route_entry, bool) {
	var best *route_entry
	for _, entry := range t.by_domain[domain] {
		if strings.HasPrefix(path, entry.prefix) && (best == nil || len(entry.prefix) > len(best.prefix)) {
			best = entry
		}
	}
	return best, best != nil
}
