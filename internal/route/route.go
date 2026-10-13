package route

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Protocol 请求协议取值：配置的 protocol 字段、请求目标 scheme 与上游 URL scheme 共用。
const (
	ProtocolHTTP  = "http"
	ProtocolHTTPS = "https"
)

// Route 一条路径前缀到转发目标的映射。handler 在加载配置时构建完毕，
// 运行期只做前缀匹配，不再区分远程上游与本地目录。
type Route struct {
	Prefix  string
	Host    string
	Headers HeaderRewrite
	Target  *Target
	Status  int
	Body    string

	handler http.Handler
}

// Handler 返回加载配置时构建好的处理器。
func (r *Route) Handler() http.Handler { return r.handler }

func (r *Route) fingerprint() string {
	target := ""
	if r.Target != nil {
		target = r.Target.Summary
	}
	return strings.Join([]string{
		r.Prefix,
		target,
		strconv.Itoa(r.Status),
		r.Body,
		r.Host,
		headerFingerprint(r.Headers.Request),
		headerFingerprint(r.Headers.Response),
	}, "\x00") + "\n"
}

func headerFingerprint(headers map[string]string) string {
	keys := slices.Sorted(maps.Keys(headers))
	var buffer strings.Builder
	for _, key := range keys {
		buffer.WriteString(key)
		buffer.WriteByte('=')
		buffer.WriteString(headers[key])
		buffer.WriteByte('\x00')
	}
	return buffer.String()
}

// Entry 一个虚拟服务器：按 protocol 与 port 筛选请求，
// 命中的条目内路由按最长前缀匹配。Protocol 留空匹配任意协议，Port 为 0 匹配任意端口。
type Entry struct {
	Protocol string
	Port     int

	routes []*Route
}

func (e *Entry) matches(protocol string, port int) bool {
	if e.Protocol != "" && e.Protocol != protocol {
		return false
	}
	return e.Port == 0 || e.Port == port
}

// Table 按域名索引的虚拟服务器表。表在加载配置时一次性构建完毕，
// 运行期只读，热加载以整体替换实现。
type Table struct {
	byDomain map[string][]*Entry
}

func NewTable() *Table {
	return &Table{byDomain: map[string][]*Entry{}}
}

// Add 把归一后的虚拟服务器条目挂入对应域名的列表，同一域名下
// (protocol, port) 组合唯一，重复时直接报错。
func (t *Table) Add(domain string, entry *Entry, routes []*Route) error {
	for _, existing := range t.byDomain[domain] {
		if existing.Protocol == entry.Protocol && existing.Port == entry.Port {
			return fmt.Errorf("domain %q: duplicate server entry protocol=%s port=%d", domain, protocolLabel(entry.Protocol), entry.Port)
		}
	}
	entry.routes = routes
	t.byDomain[domain] = append(t.byDomain[domain], entry)
	return nil
}

// Pick 在该域名的虚拟服务器中筛选协议与端口匹配的条目，再按最长前缀匹配路由。
func (t *Table) Pick(protocol string, port int, domain, path string) (*Route, bool) {
	var best *Route
	for _, entry := range t.byDomain[domain] {
		if !entry.matches(protocol, port) {
			continue
		}
		for _, route := range entry.routes {
			if pathHasPrefix(path, route.Prefix) && (best == nil || len(route.Prefix) > len(best.Prefix)) {
				best = route
			}
		}
	}
	return best, best != nil
}

// Has 判断该协议的该端口下是否存在该域名的路由，供 CONNECT 隧道区分 MITM 与透传
func (t *Table) Has(protocol string, port int, domain string) bool {
	for _, entry := range t.byDomain[domain] {
		if entry.matches(protocol, port) {
			return true
		}
	}
	return false
}

// InstallHandlers 为每条路由构建处理器，构建完成后路由表内容不再变化。
func (t *Table) InstallHandlers(transport *http.Transport) {
	for _, entries := range t.byDomain {
		for _, entry := range entries {
			for _, route := range entry.routes {
				route.handler = route.buildHandler(transport)
			}
		}
	}
}

// Fingerprint 生成路由表的稳定指纹，用于判断热加载后的配置是否真正变化：
// 无变化时跳过重建处理器与拆掉存量连接。端口与协议纳入指纹，
// 使端口/协议调整同样触发重建。
func (t *Table) Fingerprint() string {
	var buffer strings.Builder
	for _, domain := range slices.Sorted(maps.Keys(t.byDomain)) {
		buffer.WriteString(domain)
		buffer.WriteByte('\x00')
		for _, entry := range t.byDomain[domain] {
			fmt.Fprintf(&buffer, "%s\x00%d\x00", entry.Protocol, entry.Port)
			for _, route := range entry.routes {
				buffer.WriteString(route.fingerprint())
			}
		}
	}
	return buffer.String()
}

// protocolLabel 用于配置错误消息：省略 protocol 的配置项标记为 any，便于对照配置定位。
func protocolLabel(protocol string) string {
	if protocol == "" {
		return "any"
	}
	return protocol
}

// pathHasPrefix 按路径段边界匹配前缀：/api 匹配 /api 和 /api/x，不匹配 /api-v2
func pathHasPrefix(path, prefix string) bool {
	if prefix == "/" {
		return true
	}
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	if strings.HasSuffix(prefix, "/") {
		return true
	}
	return len(path) == len(prefix) || path[len(prefix)] == '/'
}
