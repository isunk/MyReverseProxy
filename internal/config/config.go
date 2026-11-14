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

// Config 顶层配置：Servers 为按域名分组的虚拟服务器，Nameservers 为上游域名解析用的 DNS 服务器。
type Config struct {
	Servers     []Server `yaml:"servers"`
	Nameservers []string `yaml:"nameservers"`
}

// Server 一个虚拟服务器及其路由条目；protocol 与 port 省略时匹配任意取值。
type Server struct {
	Domain   string  `yaml:"domain"`
	Port     int     `yaml:"port"`
	Protocol string  `yaml:"protocol"` // 仅支持 HTTP/HTTPS 反向代理，不提供 TCP/UDP 端口转发。
	Routes   []Route `yaml:"routes"`
}

// Route 一条路径前缀到上游的映射，按最长前缀匹配；Status 非零时直接返回该状态码与 Body，不转发上游。
type Route struct {
	Prefix   string  `yaml:"prefix"`
	Upstream string  `yaml:"upstream"`
	Status   int     `yaml:"status"`
	Body     string  `yaml:"body"`
	Host     string  `yaml:"host"`
	Headers  Headers `yaml:"headers"`
}

// Headers 转发时改写的消息头：Request 写入发往上游，Response 覆盖下游返回（如跨域校验头），Set 语义覆盖同名。
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

// buildTable 把配置构建为按声明顺序排列的域名分组表，同一 domain 值可按 (protocol, port) 组合声明多条。
func buildTable(config *Config) (*route.Table, error) {
	table := route.NewTable()
	for i, server := range config.Servers {
		name := strings.ToLower(strings.TrimSpace(server.Domain))
		if name == "" {
			return nil, fmt.Errorf("servers[%d]: domain is required", i)
		}
		if err := validateDomainPattern(name); err != nil {
			return nil, fmt.Errorf("servers[%d] domain %q: %w", i, server.Domain, err)
		}
		entry, err := buildServerEntry(i, server)
		if err != nil {
			return nil, err
		}
		routes, err := buildRoutes(i, server)
		if err != nil {
			return nil, err
		}
		if err := table.Add(name, entry, routes); err != nil {
			return nil, fmt.Errorf("servers[%d]: %w", i, err)
		}
	}
	return table, nil
}

// validateDomainPattern 校验归一后的 domain 值：通配符仅允许 *（匹配任意域名）与 *.suffix（匹配单层子域），其余位置出现 * 均视为配置错误。
func validateDomainPattern(name string) error {
	if name == "*" {
		return nil
	}
	if strings.HasPrefix(name, "*.") {
		if name == "*." {
			return errors.New(`suffix is empty, use "*" to match all domains`)
		}
		if strings.Contains(name[2:], "*") {
			return errors.New("wildcard must be the leading label")
		}
		return nil
	}
	if strings.Contains(name, "*") {
		return errors.New("wildcard must be the leading label")
	}
	return nil
}

// buildServerEntry 归一 server 条目的协议与端口：protocol 省略匹配任意协议，port 为 0 匹配任意端口，显式取值须 1-65535。
func buildServerEntry(index int, server Server) (*route.Entry, error) {
	protocol := strings.ToLower(strings.TrimSpace(server.Protocol))
	switch protocol {
	case "", route.ProtocolHTTP, route.ProtocolHTTPS:
	default:
		return nil, fmt.Errorf("servers[%d] domain %q protocol %q is not supported", index, server.Domain, server.Protocol)
	}
	port := server.Port
	if port != 0 && (port < 1 || port > 65535) {
		return nil, fmt.Errorf("servers[%d] domain %q port %d must be 1-65535", index, server.Domain, server.Port)
	}
	return &route.Entry{Protocol: protocol, Port: port}, nil
}

// buildRoutes 构建单个虚拟服务器的路由：前缀必填且以 / 开头、条目内不可重复，status 与 upstream 互斥。
func buildRoutes(index int, server Server) ([]*route.Route, error) {
	seen := map[string]bool{}
	entries := make([]*route.Route, 0, len(server.Routes))
	for j, configured := range server.Routes {
		prefix := strings.TrimSpace(configured.Prefix)
		upstream := strings.TrimSpace(configured.Upstream)
		status := configured.Status
		where := fmt.Sprintf("servers[%d] domain %q route[%d]", index, server.Domain, j)
		if prefix == "" {
			return nil, fmt.Errorf("%s: prefix is required", where)
		}
		if !strings.HasPrefix(prefix, "/") {
			return nil, fmt.Errorf("%s prefix %q must start with /", where, prefix)
		}
		if status != 0 && upstream != "" {
			return nil, fmt.Errorf("%s prefix %q status and upstream are mutually exclusive", where, prefix)
		}
		if status == 0 && upstream == "" {
			return nil, fmt.Errorf("%s prefix %q status or upstream is required", where, prefix)
		}
		// 1xx 在 net/http 里是 informational 响应，status 路由配了拿不到最终响应，直接拒绝
		if status != 0 && (status < 200 || status > 599) {
			return nil, fmt.Errorf("%s prefix %q status %d must be 200-599", where, prefix, status)
		}
		// 204/205/304 按协议不得携带消息体：204/304 Go 服务端会静默丢弃 body，205 会实际发送，配置了均无效
		if configured.Body != "" && (status == 204 || status == 205 || status == 304) {
			return nil, fmt.Errorf("%s prefix %q status %d must not carry a body", where, prefix, status)
		}
		if seen[prefix] {
			return nil, fmt.Errorf("%s prefix %q is duplicated", where, prefix)
		}
		seen[prefix] = true
		r := &route.Route{
			Prefix: prefix,
			Host:   strings.TrimSpace(configured.Host),
			Headers: route.HeaderRewrite{
				Request:  configured.Headers.Request,
				Response: configured.Headers.Response,
			},
		}
		if status != 0 {
			r.Status = status
			r.Body = configured.Body
		} else {
			target, err := route.ParseTarget(upstream)
			if err != nil {
				return nil, fmt.Errorf("%s prefix %q upstream %q: %w", where, prefix, upstream, err)
			}
			r.Target = target
		}
		entries = append(entries, r)
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
	if err := os.WriteFile(path, defaultConfig, 0o644); err != nil {
		return err
	}
	log.Info("config created path=%s", path)
	return nil
}
