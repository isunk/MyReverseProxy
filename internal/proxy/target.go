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

// targetOf 从请求提取转发目标的协议、端口与域名：CONNECT 按 https 处理；
// 代理绝对形式请求取 URL 的 scheme 与目标主机；隧道内层收到的源形式请求
// 取 Host 头并按连接是否为 TLS 判定协议。省略端口时按协议补默认端口。
func targetOf(request *http.Request) (string, int, string) {
	if request.Method == http.MethodConnect {
		host, port := hostAndPort(request.Host, defaultHTTPSPort)
		return route.ProtocolHTTPS, port, strings.ToLower(host)
	}
	if request.URL.Scheme == "" {
		protocol := route.ProtocolHTTP
		if request.TLS != nil {
			protocol = route.ProtocolHTTPS
		}
		host, port := hostAndPort(request.Host, defaultPort(protocol))
		return protocol, port, strings.ToLower(host)
	}
	return request.URL.Scheme, effectivePort(request.URL), hostOnly(request.URL.Host)
}

// hostAndPort 拆分 Host 为名称与端口，端口缺失或非法时补 fallback。
// 无端口的 [::1] 括号形态剥掉括号，使域名键与配置写法（::1）及 JoinHostPort 语义对齐。
func hostAndPort(host string, fallback int) (string, int) {
	name, port, err := net.SplitHostPort(host)
	if err != nil || name == "" {
		name = trimIPv6Brackets(host)
	}
	if number, err := strconv.Atoi(port); err == nil && number >= 1 && number <= 65535 {
		return name, number
	}
	return name, fallback
}

// hostOnly 取 Host 头的主机名部分并小写化，便于与路由表键对齐。
func hostOnly(host string) string {
	name, _, err := net.SplitHostPort(host)
	if err == nil {
		return strings.ToLower(name)
	}
	return strings.ToLower(trimIPv6Brackets(host))
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

// effectivePort 取目标主机端口，省略时按协议补默认端口。
func effectivePort(targetURL *url.URL) int {
	if port, err := strconv.Atoi(targetURL.Port()); err == nil {
		return port
	}
	return defaultPort(targetURL.Scheme)
}
