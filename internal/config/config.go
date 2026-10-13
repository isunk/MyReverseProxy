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
	Protocol string  `yaml:"protocol"`
	Routes   []Route `yaml:"routes"`
}

// Route 一条路径前缀到上游的映射，命中最长前缀时生效。
// Status 非零时直接返回该状态码与 Body，不转发到 upstream。
type Route struct {
	Prefix   string  `yaml:"prefix"`
	Upstream string  `yaml:"upstream"`
	Status   int     `yaml:"status"`
	Body     string  `yaml:"body"`
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
	case "", route.ProtocolHTTP, route.ProtocolHTTPS:
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
// status 与 upstream 互斥：status 非零时直接返回状态码，不解析 upstream。
func buildRoutes(server Server) ([]*route.Route, error) {
	seen := map[string]bool{}
	entries := make([]*route.Route, 0, len(server.Routes))
	for _, configured := range server.Routes {
		prefix := strings.TrimSpace(configured.Prefix)
		upstream := strings.TrimSpace(configured.Upstream)
		status := configured.Status
		if prefix == "" {
			return nil, fmt.Errorf("domain %q: prefix is required", server.Domain)
		}
		if !strings.HasPrefix(prefix, "/") {
			return nil, fmt.Errorf("domain %q: prefix %q must start with /", server.Domain, prefix)
		}
		if status != 0 && upstream != "" {
			return nil, fmt.Errorf("domain %q: prefix %q: status and upstream are mutually exclusive", server.Domain, prefix)
		}
		if status == 0 && upstream == "" {
			return nil, fmt.Errorf("domain %q: prefix %q: status or upstream is required", server.Domain, prefix)
		}
		if status != 0 && (status < 100 || status > 599) {
			return nil, fmt.Errorf("domain %q: prefix %q: status %d out of range (100-599)", server.Domain, prefix, status)
		}
		if seen[prefix] {
			return nil, fmt.Errorf("domain %q: duplicate prefix %q", server.Domain, prefix)
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
				return nil, fmt.Errorf("domain %q: %w", server.Domain, err)
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
	log.Info("created default config file path=%s", path)
	return nil
}
