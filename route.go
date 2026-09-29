package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
)

type route struct {
	prefix          string
	target          *url.URL
	fileRoot        string
	host            string
	requestHeaders  map[string]string
	responseHeaders map[string]string
	proxy           *httputil.ReverseProxy
	fileServer      http.Handler
}

// upstream 返回转发目标的描述文本：本地目录回显路径，远程上游回显 URI。
func (r *route) upstream() string {
	if r.fileRoot != "" {
		return r.fileRoot
	}
	return r.target.String()
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

// fingerprint 生成路由表的稳定指纹，用于判断热加载后的配置是否真正变化：
// 无变化时跳过重建处理器与拆掉存量连接。
func (t *routeTable) fingerprint() string {
	domains := make([]string, 0, len(t.byDomain))
	for domain := range t.byDomain {
		domains = append(domains, domain)
	}
	slices.Sort(domains)
	var buffer strings.Builder
	for _, domain := range domains {
		buffer.WriteString(domain)
		buffer.WriteByte('\x00')
		for _, entry := range t.byDomain[domain] {
			target := ""
			if entry.target != nil {
				target = entry.target.String()
			}
			for _, field := range []string{entry.prefix, target, entry.fileRoot, entry.host,
				headerFingerprint(entry.requestHeaders), headerFingerprint(entry.responseHeaders)} {
				buffer.WriteString(field)
				buffer.WriteByte('\x00')
			}
			buffer.WriteByte('|')
		}
	}
	return buffer.String()
}

func headerFingerprint(headers map[string]string) string {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var buffer strings.Builder
	for _, key := range keys {
		buffer.WriteString(key)
		buffer.WriteByte('=')
		buffer.WriteString(headers[key])
		buffer.WriteByte('\x00')
	}
	return buffer.String()
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
