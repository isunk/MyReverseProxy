package route

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Target 转发目标：URL 与 Root 互斥非空者生效；Summary 预计算目标描述供日志直接打印。
type Target struct {
	URL     *url.URL
	Root    string
	Summary string
}

// ParseTarget 解析上游：带 scheme 的按 URL（仅 http/https）远程转发，含路径分隔符的按本地目录托管，其余按 host[:port] 简写以 http 转发。
func ParseTarget(upstream string) (*Target, error) {
	if strings.Contains(upstream, "://") {
		parsed, err := url.Parse(upstream)
		if err != nil {
			return nil, fmt.Errorf("upstream %q is invalid: %w", upstream, err)
		}
		if parsed.Scheme != ProtocolHTTP && parsed.Scheme != ProtocolHTTPS {
			return nil, fmt.Errorf("upstream %q scheme %q is not supported", upstream, parsed.Scheme)
		}
		if parsed.Host == "" {
			return nil, fmt.Errorf("upstream %q is missing host", upstream)
		}
		return &Target{URL: parsed, Summary: parsed.String()}, nil
	}
	if isLocalPath(upstream) {
		return &Target{Root: upstream, Summary: upstream}, nil
	}
	return parseTargetHost(upstream)
}

// parseTargetHost 解析省略 scheme 的 host[:port] 简写统一按 http 转发，支持 host、host:port、[IPv6]:port 写法。
func parseTargetHost(upstream string) (*Target, error) {
	remote := url.URL{Scheme: ProtocolHTTP}
	if host, port, err := net.SplitHostPort(upstream); err == nil {
		if host == "" {
			return nil, fmt.Errorf("upstream %q is missing host", upstream)
		}
		if !validPort(port) {
			return nil, fmt.Errorf("upstream %q port must be 1-65535", upstream)
		}
		remote.Host = upstream
	} else if strings.Contains(upstream, ":") {
		return nil, fmt.Errorf("upstream %q is malformed, use host or [host]:port", upstream)
	} else {
		remote.Host = upstream
	}
	return &Target{URL: &remote, Summary: remote.String()}, nil
}

// validPort 校验端口为 1-65535 的纯十进制
func validPort(port string) bool {
	number, err := strconv.Atoi(port)
	return err == nil && strconv.Itoa(number) == port && number >= 1 && number <= 65535
}

// isLocalPath 判断 upstream 是否为本地目录：含 / 或 \ 分隔符即视为目录（覆盖盘符、UNC 路径），"." 与 ".." 也按目录处理，其余裸词按远程上游解析。
func isLocalPath(upstream string) bool {
	return upstream == "." || upstream == ".." || strings.ContainsAny(upstream, "/\\")
}
