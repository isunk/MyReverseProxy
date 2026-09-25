package main

import (
	"fmt"
	"net/url"
	"os"

	"gopkg.in/yaml.v3"
)

const defaultConfig = `# mrp 反向代理路由配置
# 修改后自动热加载，无需重启。
#
# domain:     按域名精确匹配，HTTPS 用 SNI、HTTP 用 Host 头
# prefix:     路径前缀，按最长前缀匹配转发到 upstream
# upstream:   上游服务地址，路径前缀自动映射
# host:       可选，改写转发时的 Host 头
# tls_verify: 可选，默认 true，false 跳过 HTTPS 上游证书校验
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
	Prefix    string `yaml:"prefix"`
	Upstream  string `yaml:"upstream"`
	Host      string `yaml:"host"`
	TLSVerify *bool  `yaml:"tls_verify"`
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
		if server.Domain == "" {
			return nil, fmt.Errorf("domain 不能为空")
		}
		if _, exists := table.byDomain[server.Domain]; exists {
			return nil, fmt.Errorf("domain %q 重复定义", server.Domain)
		}
		seen := map[string]bool{}
		var entries []*route
		for _, routeConfig := range server.Routes {
			if routeConfig.Prefix == "" || routeConfig.Upstream == "" {
				return nil, fmt.Errorf("domain %q: prefix 与 upstream 均为必填", server.Domain)
			}
			if seen[routeConfig.Prefix] {
				return nil, fmt.Errorf("domain %q: prefix %q 重复定义", server.Domain, routeConfig.Prefix)
			}
			seen[routeConfig.Prefix] = true
			target, err := url.Parse(routeConfig.Upstream)
			if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
				return nil, fmt.Errorf("domain %q: 非法 upstream %q", server.Domain, routeConfig.Upstream)
			}
			entries = append(entries, &route{
				prefix:   routeConfig.Prefix,
				target:   target,
				host:     routeConfig.Host,
				insecure: routeConfig.TLSVerify != nil && !*routeConfig.TLSVerify,
			})
		}
		table.byDomain[server.Domain] = entries
	}
	return table, nil
}
