package proxy

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"mrp/internal/netutil"
	"mrp/internal/route"
)

// 省略端口时按协议补齐的默认端口
const (
	defaultHTTPPort  = 80
	defaultHTTPSPort = 443
)

// targetOf 从请求提取转发目标的协议、端口与域名：CONNECT 按 https，代理绝对形式取 scheme 与主机。
func targetOf(request *http.Request) (string, int, string) {
	if request.Method == http.MethodConnect {
		host, port := hostAndPort(request.Host, defaultHTTPSPort)
		return route.ProtocolHTTPS, port, netutil.NormalizeDomain(host)
	}
	if request.URL.Scheme == "" {
		protocol := route.ProtocolHTTP
		if request.TLS != nil {
			protocol = route.ProtocolHTTPS
		}
		host, port := hostAndPort(request.Host, defaultPort(protocol))
		return protocol, port, netutil.NormalizeDomain(host)
	}
	return normalizeProtocol(request.URL.Scheme), effectivePort(request.URL), hostOnly(request.URL.Host)
}

// normalizeProtocol 归一请求目标 scheme 为小写：RFC 7230 允许大小写不敏感，需与路由协议一致。
func normalizeProtocol(scheme string) string {
	return strings.ToLower(scheme)
}

// absoluteTarget 把协议、域名、端口与请求 URL 拼成完整形式 URL，供路由日志与透传共用。
func absoluteTarget(protocol string, port int, domain string, requestURL *url.URL) string {
	return protocol + "://" + net.JoinHostPort(domain, strconv.Itoa(port)) + requestURL.RequestURI()
}

// hostAndPort 拆分 Host 为名称与端口，端口缺失或非法补 fallback，无端口的 [::1] 剥括号。
func hostAndPort(host string, fallback int) (string, int) {
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		name = trimIPv6Brackets(host)
	}
	if number, ok := netutil.ParsePort(port); ok {
		return name, number
	}
	return name, fallback
}

// hostOnly 取 Host 头的主机名部分并归一化。
func hostOnly(host string) string {
	name, _, err := net.SplitHostPort(host)
	if err == nil {
		return netutil.NormalizeDomain(name)
	}
	return netutil.NormalizeDomain(trimIPv6Brackets(host))
}

// trimIPv6Brackets 剥掉仅由一对括号包裹的 IPv6 字面量，其余形态原样返回。
func trimIPv6Brackets(host string) string {
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		return host[1 : len(host)-1]
	}
	return host
}

// defaultPort 按协议补齐省略的目标端口
func defaultPort(protocol string) int {
	if protocol == route.ProtocolHTTPS {
		return defaultHTTPSPort
	}
	return defaultHTTPPort
}

// effectivePort 取目标主机端口，省略、非法或越界（非 1-65535）时按协议补默认端口。
func effectivePort(targetURL *url.URL) int {
	if number, ok := netutil.ParsePort(targetURL.Port()); ok {
		return number
	}
	return defaultPort(normalizeProtocol(targetURL.Scheme))
}
