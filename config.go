package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultConfig = `# mrp 反向代理路由配置
# 修改后自动热加载，无需重启。
#
# domain:     按域名精确匹配，HTTPS 用 SNI、HTTP 用 Host 头
# prefix:     路径前缀，按最长前缀匹配转发到 upstream
# upstream:   上游服务地址，路径前缀自动映射；也支持本地目录路径（相对/绝对，含 Windows 盘符如 C:\，目录命中回退 index.html）
# host:       可选，改写转发时的 Host 头
# headers:    可选，改写消息头（Set 语义，覆盖同名已有值）
#   request:  发往上游的请求头
#   response: 覆盖下游返回的响应头（如跨域校验 Access-Control-Allow-*）
#
# 示例：
# servers:
#   - domain: api.target-app.com
#     routes:
#       - prefix: /v1/
#         upstream: https://api.our-server.com/v1/
#         host: api.our-server.com
#         headers:
#           request:
#             Authorization: "Bearer token"
#           response:
#             Access-Control-Allow-Origin: "*"
#             Access-Control-Allow-Methods: "GET, POST, OPTIONS"
#       - prefix: /assets/
#         upstream: ./dist
#       - prefix: /
#         upstream: http://192.168.1.50:8080

servers: []
`

type Config struct {
	Servers []Server `yaml:"servers"`
}

type Server struct {
	Domain string  `yaml:"domain"`
	Routes []Route `yaml:"routes"`
}

type Route struct {
	Prefix   string       `yaml:"prefix"`
	Upstream string       `yaml:"upstream"`
	Host     string       `yaml:"host"`
	Headers  RouteHeaders `yaml:"headers"`
}

// RouteHeaders 在转发时改写消息头：request 写入发往上游的请求头，
// response 覆盖下游返回的响应头（如跨域校验头）。Set 语义，覆盖同名已有值。
type RouteHeaders struct {
	Request  map[string]string `yaml:"request"`
	Response map[string]string `yaml:"response"`
}

func loadTable(path string) (*routeTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	table := &routeTable{byDomain: map[string][]*route{}}
	for _, server := range config.Servers {
		domain := strings.ToLower(strings.TrimSpace(server.Domain))
		if domain == "" {
			return nil, errors.New("domain must not be empty")
		}
		if _, exists := table.byDomain[domain]; exists {
			return nil, fmt.Errorf("duplicate domain %q", server.Domain)
		}
		entries, err := buildRoutes(server)
		if err != nil {
			return nil, err
		}
		table.byDomain[domain] = entries
	}
	return table, nil
}

func buildRoutes(server Server) ([]*route, error) {
	seen := map[string]bool{}
	entries := make([]*route, 0, len(server.Routes))
	for _, routeConfig := range server.Routes {
		prefix := strings.TrimSpace(routeConfig.Prefix)
		if prefix == "" || routeConfig.Upstream == "" {
			return nil, fmt.Errorf("domain %q: prefix and upstream are required", server.Domain)
		}
		if !strings.HasPrefix(prefix, "/") {
			return nil, fmt.Errorf("domain %q: prefix %q must start with /", server.Domain, prefix)
		}
		if seen[prefix] {
			return nil, fmt.Errorf("domain %q: duplicate prefix %q", server.Domain, prefix)
		}
		seen[prefix] = true
		entry := &route{
			prefix:          prefix,
			host:            strings.TrimSpace(routeConfig.Host),
			requestHeaders:  routeConfig.Headers.Request,
			responseHeaders: routeConfig.Headers.Response,
		}
		target, err := url.Parse(routeConfig.Upstream)
		if err != nil {
			return nil, fmt.Errorf("domain %q: invalid upstream %q: %w", server.Domain, routeConfig.Upstream, err)
		}
		switch {
		case target.Scheme == "http" || target.Scheme == "https":
			if target.Host == "" {
				return nil, fmt.Errorf("domain %q: upstream %q is missing host", server.Domain, routeConfig.Upstream)
			}
			entry.target = target
		case isLocalPath(routeConfig.Upstream):
			entry.fileRoot = routeConfig.Upstream
		default:
			return nil, fmt.Errorf("domain %q: unsupported upstream scheme %q", server.Domain, routeConfig.Upstream)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// isLocalPath 判断 upstream 是否为本地目录路径：无 scheme 的相对/绝对路径，
// 以及 Windows 盘符绝对路径（如 C:\ 或 D:/）。url.Parse 会把 "D:\web" 误判为
// scheme "d"，故在此显式识别盘符与无冒号路径，交给 http.Dir 托管。
func isLocalPath(upstream string) bool {
	if !strings.Contains(upstream, ":") {
		return true
	}
	if len(upstream) >= 3 {
		c := upstream[0]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			if upstream[1] == ':' && (upstream[2] == '\\' || upstream[2] == '/') {
				return true
			}
		}
	}
	return false
}
