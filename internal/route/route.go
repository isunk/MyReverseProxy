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

// Route 一条路径前缀到转发目标的映射；handler 加载配置时构建完毕，运行期只做前缀匹配。
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

// Entry 一个虚拟服务器：按 protocol 与 port 筛选请求，条目内路由按最长前缀匹配；Protocol 留空匹配任意协议，Port 为 0 匹配任意端口。
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

// Table 按域名模式分组的虚拟服务器表，分组按配置声明顺序排列、先声明先匹配；
// 加载配置时一次性构建、运行期只读，热加载以整体替换实现。
type Table struct {
	groups []*DomainGroup
}

// DomainGroup 一个域名模式下的全部虚拟服务器条目：精确分组按域名相等匹配，
// 通配符分组按 RFC 1034 单层子域语义匹配。
type DomainGroup struct {
	pattern  string
	suffix   string
	wildcard bool
	entries  []*Entry
}

// matches 判断该分组是否响应 (protocol, port, domain) 目标：域名模式命中且分组内存在匹配条目。
func (g *DomainGroup) matches(protocol string, port int, domain string) bool {
	if g.wildcard {
		if !wildcardMatch(domain, g.suffix) {
			return false
		}
	} else if g.pattern != domain {
		return false
	}
	for _, entry := range g.entries {
		if entry.matches(protocol, port) {
			return true
		}
	}
	return false
}

// wildcardMatch 判定请求域名是否命中通配符模式：suffix 为空表示 * 匹配任意域名；
// *.suffix 按 RFC 1034 语义匹配单层子域 label.suffix，裸域与多层子域不命中。
func wildcardMatch(domain, suffix string) bool {
	if domain == "" {
		return false
	}
	if suffix == "" {
		return true
	}
	label, ok := strings.CutSuffix(domain, "."+suffix)
	return ok && label != "" && !strings.Contains(label, ".")
}

// newDomainGroup 解析归一后的 domain 配置值：* 匹配任意域名，*.suffix 匹配单层子域，其余为精确域名。
func newDomainGroup(pattern string, entry *Entry) *DomainGroup {
	group := &DomainGroup{pattern: pattern, entries: []*Entry{entry}}
	if pattern == "*" {
		group.wildcard = true
	} else if strings.HasPrefix(pattern, "*.") {
		group.wildcard = true
		group.suffix = pattern[2:]
	}
	return group
}

func NewTable() *Table {
	return &Table{}
}

// Add 把归一后的虚拟服务器条目挂入对应域名分组，分组按首次声明顺序排列，同一 domain 值多次声明合并为一组；
// 同一分组内 (protocol, port) 组合唯一，重复报错。
func (t *Table) Add(domain string, entry *Entry, routes []*Route) error {
	for _, group := range t.groups {
		if group.pattern != domain {
			continue
		}
		for _, existing := range group.entries {
			if existing.Protocol == entry.Protocol && existing.Port == entry.Port {
				return fmt.Errorf("domain %q server entry is duplicated protocol=%s port=%d", domain, protocolLabel(entry.Protocol), entry.Port)
			}
		}
		entry.routes = routes
		group.entries = append(group.entries, entry)
		return nil
	}
	entry.routes = routes
	t.groups = append(t.groups, newDomainGroup(domain, entry))
	return nil
}

// Pick 按声明顺序选出第一个命中 (protocol, port, domain) 的分组，再在其条目内按最长前缀匹配路由。
func (t *Table) Pick(protocol string, port int, domain, path string) (*Route, bool) {
	for _, group := range t.groups {
		if !group.matches(protocol, port, domain) {
			continue
		}
		var best *Route
		for _, entry := range group.entries {
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
	return nil, false
}

// Has 判断是否存在命中 (protocol, port, domain) 的分组，供 CONNECT 隧道区分 MITM 与透传
func (t *Table) Has(protocol string, port int, domain string) bool {
	for _, group := range t.groups {
		if group.matches(protocol, port, domain) {
			return true
		}
	}
	return false
}

// InstallHandlers 为每条路由构建处理器，构建完成后路由表内容不再变化。
func (t *Table) InstallHandlers(transport *http.Transport) {
	for _, group := range t.groups {
		for _, entry := range group.entries {
			for _, route := range entry.routes {
				route.handler = route.buildHandler(transport)
			}
		}
	}
}

// Fingerprint 生成路由表稳定指纹用于判断热加载是否变化：按声明顺序输出，声明顺序变化反映到指纹并触发重建；端口与协议也纳入以触发重建。
func (t *Table) Fingerprint() string {
	var buffer strings.Builder
	for _, group := range t.groups {
		buffer.WriteString(group.pattern)
		buffer.WriteByte('\x00')
		for _, entry := range group.entries {
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
