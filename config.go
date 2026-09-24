package main

import (
	"fmt"
	"net/url"
	"os"

	"gopkg.in/yaml.v3"
)

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
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	table := &routeTable{byDomain: map[string][]*route{}}
	for _, server := range cfg.Servers {
		if server.Domain == "" {
			return nil, fmt.Errorf("domain 不能为空")
		}
		if _, exists := table.byDomain[server.Domain]; exists {
			return nil, fmt.Errorf("domain %q 重复定义", server.Domain)
		}
		seen := map[string]bool{}
		var entries []*route
		for _, routeCfg := range server.Routes {
			if routeCfg.Prefix == "" || routeCfg.Upstream == "" {
				return nil, fmt.Errorf("domain %q: prefix 与 upstream 均为必填", server.Domain)
			}
			if seen[routeCfg.Prefix] {
				return nil, fmt.Errorf("domain %q: prefix %q 重复定义", server.Domain, routeCfg.Prefix)
			}
			seen[routeCfg.Prefix] = true
			target, err := url.Parse(routeCfg.Upstream)
			if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
				return nil, fmt.Errorf("domain %q: 非法 upstream %q", server.Domain, routeCfg.Upstream)
			}
			entries = append(entries, &route{
				prefix:   routeCfg.Prefix,
				target:   target,
				host:     routeCfg.Host,
				insecure: routeCfg.TLSVerify != nil && !*routeCfg.TLSVerify,
			})
		}
		table.byDomain[server.Domain] = entries
	}
	return table, nil
}
