package main

import (
	"bytes"
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
# nameservers: 顶层字段，上游域名解析用的 DNS 服务器，支持裸 IP（默认 53 端口）与 host:port 写法，按顺序故障切换
#   设备上的 /etc/resolv.conf 常指向 [::1]:53 等守护进程地址，mrp 读不到可用服务器时上游域名会解析失败，
#   部署到设备时必须显式配置
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
# nameservers:
#   - "114.114.114.114"
#   - "223.5.5.5"
#   - "[2001:4860:4860::8888]:5353"

servers: []
nameservers: []
`

// Config 顶层配置：Domains 为按入口域名分组的路由，Nameservers 为上游域名解析用的 DNS 服务器。
// YAML 键沿用 servers/domain，保持既有配置文件兼容。
type Config struct {
	Domains     []Domain `yaml:"servers"`
	Nameservers []string `yaml:"nameservers"`
}

// Domain 一个入口域名及其路由条目；HTTPS 按 SNI 匹配，HTTP 按 Host 头匹配。
type Domain struct {
	Name   string  `yaml:"domain"`
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

// loadTable 读取配置并构建路由表，同时返回配置声明的 DNS 服务器。
func loadTable(path string) (*routeTable, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read config: %w", err)
	}
	config, err := parseConfig(data)
	if err != nil {
		return nil, nil, err
	}
	table, err := buildTable(config)
	if err != nil {
		return nil, nil, err
	}
	return table, config.Nameservers, nil
}

// parseConfig 解析 YAML 配置；开启 KnownFields，未知字段直接报错以免配置写错被静默忽略。
func parseConfig(data []byte) (*Config, error) {
	var config Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &config, nil
}

// buildTable 把配置构建为按域名索引的路由表；域名唯一，域名内前缀唯一。
func buildTable(config *Config) (*routeTable, error) {
	table := &routeTable{byDomain: map[string][]*route{}}
	for _, domain := range config.Domains {
		name := strings.ToLower(strings.TrimSpace(domain.Name))
		if name == "" {
			return nil, errors.New("domain must not be empty")
		}
		if _, exists := table.byDomain[name]; exists {
			return nil, fmt.Errorf("duplicate domain %q", domain.Name)
		}
		entries, err := buildRoutes(domain)
		if err != nil {
			return nil, err
		}
		table.byDomain[name] = entries
	}
	return table, nil
}

// buildRoutes 构建单个域名的路由条目：前缀必填且以 / 开头，域名内不可重复。
func buildRoutes(domain Domain) ([]*route, error) {
	seen := map[string]bool{}
	entries := make([]*route, 0, len(domain.Routes))
	for _, routeConfig := range domain.Routes {
		prefix := strings.TrimSpace(routeConfig.Prefix)
		if prefix == "" || routeConfig.Upstream == "" {
			return nil, fmt.Errorf("domain %q: prefix and upstream are required", domain.Name)
		}
		if !strings.HasPrefix(prefix, "/") {
			return nil, fmt.Errorf("domain %q: prefix %q must start with /", domain.Name, prefix)
		}
		if seen[prefix] {
			return nil, fmt.Errorf("domain %q: duplicate prefix %q", domain.Name, prefix)
		}
		seen[prefix] = true
		target, err := parseUpstream(routeConfig.Upstream)
		if err != nil {
			return nil, fmt.Errorf("domain %q: %w", domain.Name, err)
		}
		entries = append(entries, &route{
			prefix: prefix,
			host:   strings.TrimSpace(routeConfig.Host),
			headers: headerRewrite{
				request:  routeConfig.Headers.Request,
				response: routeConfig.Headers.Response,
			},
			target: target,
		})
	}
	return entries, nil
}

// parseUpstream 解析上游：http(s) URI 走远程转发，本地目录路径走静态托管。
func parseUpstream(upstream string) (target, error) {
	parsed, err := url.Parse(upstream)
	if err != nil {
		return target{}, fmt.Errorf("invalid upstream %q: %w", upstream, err)
	}
	switch {
	case parsed.Scheme == "http" || parsed.Scheme == "https":
		if parsed.Host == "" {
			return target{}, fmt.Errorf("upstream %q is missing host", upstream)
		}
		return target{url: parsed, summary: parsed.String()}, nil
	case isLocalPath(upstream):
		return target{root: upstream, summary: upstream}, nil
	default:
		return target{}, fmt.Errorf("unsupported upstream scheme %q", upstream)
	}
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
