package proxy

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"mrp/internal/route"
)

// 省略端口时按协议补齐的默认端口
const (
	defaultHTTPPort  = 80
	defaultHTTPSPort = 443
)

// targetOf 从请求提取转发目标的协议、端口与域名：CONNECT 按 https 处理；代理绝对形式取 URL 的 scheme 与主机；隧道内层源形式取 Host 头并按 TLS 判定协议；省略端口按协议补默认。
func targetOf(request *http.Request) (string, int, string) {
	if request.Method == http.MethodConnect {
		host, port := hostAndPort(request.Host, defaultHTTPSPort)
		return route.ProtocolHTTPS, port, normalizeDomain(host)
	}
	if request.URL.Scheme == "" {
		protocol := route.ProtocolHTTP
		if request.TLS != nil {
			protocol = route.ProtocolHTTPS
		}
		host, port := hostAndPort(request.Host, defaultPort(protocol))
		return protocol, port, normalizeDomain(host)
	}
	return normalizeProtocol(request.URL.Scheme), effectivePort(request.URL), hostOnly(request.URL.Host)
}

// normalizeProtocol 归一请求目标 scheme 为小写：RFC 7230 允许 scheme 大小写不敏感，大写形式须与路由协议（小写）一致比较。
func normalizeProtocol(scheme string) string {
	return strings.ToLower(scheme)
}

// normalizeDomain 归一目标域名为小写并剥离 FQDN 尾点：DNS 绝对形式 foo.example.com. 与配置的 foo.example.com 视为同一域，尾点不剥离则精确与通配符分组都不命中。
func normalizeDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(domain), ".")
}

// absoluteTarget 把协议、域名、端口与请求 URL 拼成完整形式 URL，供路由日志与透传共用。
func absoluteTarget(protocol string, port int, domain string, requestURL *url.URL) string {
	return protocol + "://" + net.JoinHostPort(domain, strconv.Itoa(port)) + requestURL.RequestURI()
}

// hostAndPort 拆分 Host 为名称与端口，端口缺失或非法时补 fallback；无端口的 [::1] 括号形态剥掉括号；主机名为空（如 ":8080"）保持空，交由调用方按未匹配路由处理。
func hostAndPort(host string, fallback int) (string, int) {
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		name = trimIPv6Brackets(host)
	}
	if number, err := strconv.Atoi(port); err == nil && number >= 1 && number <= 65535 {
		return name, number
	}
	return name, fallback
}

// hostOnly 取 Host 头的主机名部分并归一化。
func hostOnly(host string) string {
	name, _, err := net.SplitHostPort(host)
	if err == nil {
		return normalizeDomain(name)
	}
	return normalizeDomain(trimIPv6Brackets(host))
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
	if port, err := strconv.Atoi(targetURL.Port()); err == nil && port >= 1 && port <= 65535 {
		return port
	}
	return defaultPort(normalizeProtocol(targetURL.Scheme))
}
