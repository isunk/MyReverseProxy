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
# upstream:   上游服务地址，路径前缀自动映射
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
#         upstream: https://our-server-a.com/v1/
#         headers:
#           request:
#             Authorization: "Bearer token"
#           response:
#             Access-Control-Allow-Origin: "*"
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
		domain := strings.ToLower(server.Domain)
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
		if routeConfig.Prefix == "" || routeConfig.Upstream == "" {
			return nil, fmt.Errorf("domain %q: prefix and upstream are required", server.Domain)
		}
		if !strings.HasPrefix(routeConfig.Prefix, "/") {
			return nil, fmt.Errorf("domain %q: prefix %q must start with /", server.Domain, routeConfig.Prefix)
		}
		if seen[routeConfig.Prefix] {
			return nil, fmt.Errorf("domain %q: duplicate prefix %q", server.Domain, routeConfig.Prefix)
		}
		seen[routeConfig.Prefix] = true
		target, err := url.Parse(routeConfig.Upstream)
		if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
			return nil, fmt.Errorf("domain %q: invalid upstream %q", server.Domain, routeConfig.Upstream)
		}
		entries = append(entries, &route{
			prefix:          routeConfig.Prefix,
			target:          target,
			host:            routeConfig.Host,
			requestHeaders:  routeConfig.Headers.Request,
			responseHeaders: routeConfig.Headers.Response,
		})
	}
	return entries, nil
}
