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
#
# 示例：
# servers:
#   - domain: api.target-app.com
#     routes:
#       - prefix: /v1/
#         upstream: https://our-server-a.com/v1/
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
	Prefix   string `yaml:"prefix"`
	Upstream string `yaml:"upstream"`
	Host     string `yaml:"host"`
}

func loadTable(path string) (*routeTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
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
			prefix: routeConfig.Prefix,
			target: target,
			host:   routeConfig.Host,
		})
	}
	return entries, nil
}
