package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
	"mrp/internal/log"
	"mrp/internal/route"
)

const defaultConfig = `# mrp 反向代理路由配置
# 修改后自动热加载，无需重启。
#
# mrp 只监听一个端口（--port），以下字段决定哪个请求归属哪个 server，不参与监听
#
# domain:     按域名精确匹配，HTTPS 用 CONNECT 目标主机、HTTP 用 Host 头
# protocol:   可选，匹配请求的协议 http 或 https，省略匹配任意协议
# port:       可选，匹配请求的目标端口，省略匹配任意端口
#             同一 (domain, protocol, port) 组合只能声明一次
# prefix:     路径前缀，按最长前缀匹配转发到 upstream
# upstream:   上游服务地址，路径前缀自动映射；写法：
#               http://host[:port][/path]  https://host[:port][/path]  完整 URL
#               host[:port]                简写，按 http 转发（如 192.168.1.50:8080）
#               目录路径或 "."、".."        本地目录（相对/绝对，含 Windows 盘符 C:\；"." 与 "./" 等价；目录命中回退 index.html）
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
#         upstream: 192.168.1.50:8080
# nameservers:
#   - "114.114.114.114"
#   - "223.5.5.5"
#   - "[2001:4860:4860::8888]:5353"

servers: []
nameservers: []
`

// Config 顶层配置：Servers 为按域名分组的虚拟服务器，Nameservers 为上游域名解析用的 DNS 服务器。
type Config struct {
	Servers     []Server `yaml:"servers"`
	Nameservers []string `yaml:"nameservers"`
}

// Server 一个虚拟服务器及其路由条目；protocol 与 port 省略时匹配任意取值。
type Server struct {
	Domain   string  `yaml:"domain"`
	Port     int     `yaml:"port"`
	Protocol string  `yaml:"protocol"`
	Routes   []Route `yaml:"routes"`
}

// Route 一条路径前缀到上游的映射，命中最长前缀时生效。
type Route struct {
	Prefix   string  `yaml:"prefix"`
	Upstream string  `yaml:"upstream"`
	Host     string  `yaml:"host"`
	Headers  Headers `yaml:"headers"`
}

// Headers 在转发时改写消息头：Request 写入发往上游的请求头，
// Response 覆盖下游返回的响应头（如跨域校验头）。Set 语义，覆盖同名已有值。
type Headers struct {
	Request  map[string]string `yaml:"request"`
	Response map[string]string `yaml:"response"`
}

// Load 读取配置并构建路由表，同时返回配置声明的 DNS 服务器。
func Load(path string) (*route.Table, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read config: %w", err)
	}
	config, err := Parse(data)
	if err != nil {
		return nil, nil, err
	}
	table, err := buildTable(config)
	if err != nil {
		return nil, nil, err
	}
	return table, config.Nameservers, nil
}

// Parse 解析 YAML 配置；开启 KnownFields，未知字段直接报错以免配置写错被静默忽略。
func Parse(data []byte) (*Config, error) {
	var config Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		// 空文件与纯注释在 yaml 里都表现为 EOF，透出原始错误会让用户无从定位
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config file is empty")
		}
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &config, nil
}

// buildTable 把配置构建为按域名索引的虚拟服务器表：(domain, protocol, port)
// 组合唯一，同一域名可声明多个条目分别匹配不同协议与端口。
func buildTable(config *Config) (*route.Table, error) {
	table := route.NewTable()
	for _, server := range config.Servers {
		name := strings.ToLower(strings.TrimSpace(server.Domain))
		if name == "" {
			return nil, errors.New("domain must not be empty")
		}
		entry, err := buildServerEntry(server)
		if err != nil {
			return nil, err
		}
		routes, err := buildRoutes(server)
		if err != nil {
			return nil, err
		}
		if err := table.Add(name, entry, routes); err != nil {
			return nil, err
		}
	}
	return table, nil
}

// buildServerEntry 归一 server 条目的协议与端口：protocol 省略留空匹配任意协议，
// port 为 0 表示匹配任意端口，显式取值须在 1-65535。
func buildServerEntry(server Server) (*route.Entry, error) {
	protocol := strings.ToLower(strings.TrimSpace(server.Protocol))
	switch protocol {
	case "", "http", "https":
	default:
		return nil, fmt.Errorf("domain %q: unsupported protocol %q", server.Domain, server.Protocol)
	}
	port := server.Port
	if port != 0 && (port < 1 || port > 65535) {
		return nil, fmt.Errorf("domain %q: invalid port %d", server.Domain, server.Port)
	}
	return &route.Entry{Protocol: protocol, Port: port}, nil
}

// buildRoutes 构建单个虚拟服务器的路由条目：前缀必填且以 / 开头，条目内不可重复。
func buildRoutes(server Server) ([]*route.Route, error) {
	seen := map[string]bool{}
	entries := make([]*route.Route, 0, len(server.Routes))
	for _, configured := range server.Routes {
		prefix := strings.TrimSpace(configured.Prefix)
		if prefix == "" || configured.Upstream == "" {
			return nil, fmt.Errorf("domain %q: prefix and upstream are required", server.Domain)
		}
		if !strings.HasPrefix(prefix, "/") {
			return nil, fmt.Errorf("domain %q: prefix %q must start with /", server.Domain, prefix)
		}
		if seen[prefix] {
			return nil, fmt.Errorf("domain %q: duplicate prefix %q", server.Domain, prefix)
		}
		seen[prefix] = true
		target, err := route.ParseTarget(configured.Upstream)
		if err != nil {
			return nil, fmt.Errorf("domain %q: %w", server.Domain, err)
		}
		entries = append(entries, &route.Route{
			Prefix: prefix,
			Host:   strings.TrimSpace(configured.Host),
			Headers: route.HeaderRewrite{
				Request:  configured.Headers.Request,
				Response: configured.Headers.Response,
			},
			Target: target,
		})
	}
	return entries, nil
}

// Ensure 在配置文件不存在时写入默认配置，已存在则不改动。
func Ensure(path string) error {
	_, err := os.Stat(path)
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(path, []byte(defaultConfig), 0o644); err != nil {
		return err
	}
	log.Info("created default config file path=%s", path)
	return nil
}
