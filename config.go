package main

import (
	"fmt"
	"net/url"
	"os"

	"gopkg.in/yaml.v3"
)

type config struct {
	Servers []server_config `yaml:"servers"`
}

type server_config struct {
	Domain string         `yaml:"domain"`
	Routes []route_config `yaml:"routes"`
}

type route_config struct {
	Prefix    string `yaml:"prefix"`
	Upstream  string `yaml:"upstream"`
	Host      string `yaml:"host"`
	TLSVerify *bool  `yaml:"tls_verify"`
}

func load_config(path string) (*route_table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	table := &route_table{by_domain: map[string][]*route_entry{}}
	for _, server := range cfg.Servers {
		if server.Domain == "" {
			return nil, fmt.Errorf("domain 不能为空")
		}
		if _, exists := table.by_domain[server.Domain]; exists {
			return nil, fmt.Errorf("domain %q 重复定义", server.Domain)
		}
		seen := map[string]bool{}
		var entries []*route_entry
		for _, route_cfg := range server.Routes {
			if route_cfg.Prefix == "" || route_cfg.Upstream == "" {
				return nil, fmt.Errorf("domain %q: prefix 与 upstream 均为必填", server.Domain)
			}
			if seen[route_cfg.Prefix] {
				return nil, fmt.Errorf("domain %q: prefix %q 重复定义", server.Domain, route_cfg.Prefix)
			}
			seen[route_cfg.Prefix] = true
			target, err := url.Parse(route_cfg.Upstream)
			if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
				return nil, fmt.Errorf("domain %q: 非法 upstream %q", server.Domain, route_cfg.Upstream)
			}
			entries = append(entries, &route_entry{
				prefix:      route_cfg.Prefix,
				target:      target,
				host:        route_cfg.Host,
				skip_verify: route_cfg.TLSVerify != nil && !*route_cfg.TLSVerify,
			})
		}
		table.by_domain[server.Domain] = entries
	}
	return table, nil
}
