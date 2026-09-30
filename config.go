package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultConfig = `# mrp 反向代理路由配置
# 修改后自动热加载，无需重启。
#
# domain:     按域名精确匹配，HTTPS 用 SNI、HTTP 用 Host 头
# protocol:   可选，入口协议 http 或 https，省略按 http
# port:       可选，监听端口，省略按协议取默认端口（http 为 80、https 为 443）
#             多个 server 可共用一个端口（协议须一致），端口内按域名区分
# prefix:     路径前缀，按最长前缀匹配转发到 upstream
# upstream:   上游服务地址，路径前缀自动映射；写法：
#               http://host[:port][/path]  https://host[:port][/path]  完整 URL
#               host[:port]                简写，按 http 转发（如 192.168.1.50:8080）
#               含 / 或 \ 的路径            本地目录（相对/绝对，含 Windows 盘符如 C:\，目录命中回退 index.html）
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
#     port: 8443
#     protocol: https
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
#   - domain: api.target-app.com
#     routes:
#       - prefix: /
#         upstream: 192.168.1.50:8080
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

// Domain 一个入口域名及其路由条目；protocol 省略按 http，port 省略按协议取默认端口。
type Domain struct {
	Name     string  `yaml:"domain"`
	Port     int     `yaml:"port"`
	Protocol string  `yaml:"protocol"`
	Routes   []Route `yaml:"routes"`
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

// buildTable 把配置构建为按端口分组的路由表：端口协议唯一，端口内域名唯一，
// 同一域名可分别声明 http 与 https 端口条目。
func buildTable(config *Config) (*routeTable, error) {
	table := &routeTable{byPort: map[int]*portGroup{}}
	for _, domain := range config.Domains {
		if err := table.add(domain); err != nil {
			return nil, err
		}
	}
	return table, nil
}

// add 归一单条 server 的协议与端口后挂入对应端口分组，并做冲突校验。
func (t *routeTable) add(domain Domain) error {
	name := strings.ToLower(strings.TrimSpace(domain.Name))
	if name == "" {
		return errors.New("domain must not be empty")
	}
	protocol, port, err := resolveEntry(domain)
	if err != nil {
		return err
	}
	group, ok := t.byPort[port]
	if !ok {
		group = &portGroup{protocol: protocol, byDomain: map[string][]*route{}}
		t.byPort[port] = group
	}
	if group.protocol != protocol {
		return fmt.Errorf("port %d: protocol conflict between %s and %s servers", port, group.protocol, protocol)
	}
	if _, exists := group.byDomain[name]; exists {
		return fmt.Errorf("port %d: duplicate domain %q", port, domain.Name)
	}
	entries, err := buildRoutes(domain)
	if err != nil {
		return err
	}
	group.byDomain[name] = entries
	return nil
}

// resolveEntry 归一 server 条目的协议与端口：protocol 省略按 http，
// port 为 0 视为省略并按协议取默认端口（http 80 / https 443）。
func resolveEntry(domain Domain) (string, int, error) {
	protocol := strings.ToLower(strings.TrimSpace(domain.Protocol))
	switch protocol {
	case "":
		protocol = protocolHTTP
	case protocolHTTP, protocolHTTPS:
	default:
		return "", 0, fmt.Errorf("domain %q: unsupported protocol %q", domain.Name, domain.Protocol)
	}
	port := domain.Port
	if port == 0 {
		port = defaultPort(protocol)
	}
	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("domain %q: invalid port %d", domain.Name, domain.Port)
	}
	return protocol, port, nil
}

// defaultPort 协议对应的默认监听端口
func defaultPort(protocol string) int {
	if protocol == protocolHTTPS {
		return 443
	}
	return 80
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

// parseUpstream 解析上游：带 scheme 前缀的按 URL（仅 http/https）远程转发，
// 含路径分隔符的按本地静态目录托管，其余按 host[:port] 简写以 http 转发。
func parseUpstream(upstream string) (target, error) {
	if strings.Contains(upstream, "://") {
		parsed, err := url.Parse(upstream)
		if err != nil {
			return target{}, fmt.Errorf("invalid upstream %q: %w", upstream, err)
		}
		if parsed.Scheme != protocolHTTP && parsed.Scheme != protocolHTTPS {
			return target{}, fmt.Errorf("unsupported upstream scheme %q", upstream)
		}
		if parsed.Host == "" {
			return target{}, fmt.Errorf("upstream %q is missing host", upstream)
		}
		return target{url: parsed, summary: parsed.String()}, nil
	}
	if isLocalPath(upstream) {
		return target{root: upstream, summary: upstream}, nil
	}
	return parseUpstreamHost(upstream)
}

// parseUpstreamHost 解析省略 scheme 的 host[:port] 简写，统一按 http 转发。
// 支持 host、host:port、[IPv6]:port 写法；裸 IPv6 须用括号并显式端口。
func parseUpstreamHost(upstream string) (target, error) {
	remote := url.URL{Scheme: protocolHTTP}
	if host, port, err := net.SplitHostPort(upstream); err == nil {
		if host == "" {
			return target{}, fmt.Errorf("upstream %q is missing host", upstream)
		}
		if !validPort(port) {
			return target{}, fmt.Errorf("invalid upstream %q: port must be 1-65535", upstream)
		}
		remote.Host = upstream
	} else if strings.Contains(upstream, ":") {
		return target{}, fmt.Errorf("invalid upstream %q: use host or [host]:port", upstream)
	} else {
		remote.Host = upstream
	}
	return target{url: &remote, summary: remote.String()}, nil
}

// validPort 校验端口为 1-65535 的纯十进制
func validPort(port string) bool {
	number, err := strconv.Atoi(port)
	return err == nil && strconv.Itoa(number) == port && number >= 1 && number <= 65535
}

// isLocalPath 判断 upstream 是否为本地目录路径：含路径分隔符（/ 或 \），
// 覆盖 ./dist、/var/www、\\server\share 与 Windows 盘符路径 C:\web。
// 不含分隔符的裸词（如 example.com 或 192.168.1.50）按远程上游解析。
func isLocalPath(upstream string) bool {
	return strings.ContainsAny(upstream, "/\\")
}
