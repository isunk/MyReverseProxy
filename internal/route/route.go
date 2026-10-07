package route

import (
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"mrp/internal/log"
)

const (
	protocolHTTP  = "http"
	protocolHTTPS = "https"
)

// HeaderRewrite 转发时改写的消息头：Request 写入发往上游的请求头，
// Response 覆盖下游返回的响应头。Set 语义，覆盖同名已有值。
type HeaderRewrite struct {
	Request  map[string]string
	Response map[string]string
}

func (h *HeaderRewrite) rewriteRequest(headers http.Header) {
	for name, value := range h.Request {
		headers.Set(name, value)
	}
}

// rewriteResponse 作为 ReverseProxy 的 ModifyResponse 钩子；
// 没有响应头改写时构建器不挂载该钩子，避免每次响应空跑。
func (h *HeaderRewrite) rewriteResponse(response *http.Response) error {
	for name, value := range h.Response {
		response.Header.Set(name, value)
	}
	return nil
}

// Target 转发目标：URL 与 Root 互斥，非空者生效。
// Summary 预计算目标描述，日志直接打印，避免请求热路径重复格式化。
type Target struct {
	URL     *url.URL
	Root    string
	Summary string
}

// ParseTarget 解析上游：带 scheme 前缀的按 URL（仅 http/https）远程转发，
// 含路径分隔符的按本地静态目录托管，其余按 host[:port] 简写以 http 转发。
func ParseTarget(upstream string) (*Target, error) {
	if strings.Contains(upstream, "://") {
		parsed, err := url.Parse(upstream)
		if err != nil {
			return nil, fmt.Errorf("invalid upstream %q: %w", upstream, err)
		}
		if parsed.Scheme != protocolHTTP && parsed.Scheme != protocolHTTPS {
			return nil, fmt.Errorf("unsupported upstream scheme %q", upstream)
		}
		if parsed.Host == "" {
			return nil, fmt.Errorf("upstream %q is missing host", upstream)
		}
		return &Target{URL: parsed, Summary: parsed.String()}, nil
	}
	if isLocalPath(upstream) {
		return &Target{Root: upstream, Summary: upstream}, nil
	}
	return parseTargetHost(upstream)
}

// parseTargetHost 解析省略 scheme 的 host[:port] 简写，统一按 http 转发。
// 支持 host、host:port、[IPv6]:port 写法；裸 IPv6 须用括号并显式端口。
func parseTargetHost(upstream string) (*Target, error) {
	remote := url.URL{Scheme: protocolHTTP}
	if host, port, err := net.SplitHostPort(upstream); err == nil {
		if host == "" {
			return nil, fmt.Errorf("upstream %q is missing host", upstream)
		}
		if !validPort(port) {
			return nil, fmt.Errorf("invalid upstream %q: port must be 1-65535", upstream)
		}
		remote.Host = upstream
	} else if strings.Contains(upstream, ":") {
		return nil, fmt.Errorf("invalid upstream %q: use host or [host]:port", upstream)
	} else {
		remote.Host = upstream
	}
	return &Target{URL: &remote, Summary: remote.String()}, nil
}

// validPort 校验端口为 1-65535 的纯十进制
func validPort(port string) bool {
	number, err := strconv.Atoi(port)
	return err == nil && strconv.Itoa(number) == port && number >= 1 && number <= 65535
}

// isLocalPath 判断 upstream 是否为本地目录路径：含路径分隔符（/ 或 \）即视为目录，
// 覆盖 ./dist、/var/www、\\server\share 与 Windows 盘符路径 C:\web；
// "." 与 ".." 不含分隔符但同指当前/父目录，也按本地目录处理，
// 否则会被当作主机名远程代理。
// 其余裸词（如 example.com 或 192.168.1.50）按远程上游解析。
func isLocalPath(upstream string) bool {
	return upstream == "." || upstream == ".." || strings.ContainsAny(upstream, "/\\")
}

// Route 一条路径前缀到转发目标的映射。handler 在加载配置时构建完毕，
// 运行期只做前缀匹配，不再区分远程上游与本地目录。
type Route struct {
	Prefix  string
	Host    string
	Headers HeaderRewrite
	Target  *Target

	handler http.Handler
}

// Handler 返回加载配置时构建好的处理器。
func (r *Route) Handler() http.Handler { return r.handler }

func (r *Route) buildHandler(transport *http.Transport) http.Handler {
	if r.Target.Root != "" {
		return newStaticHandler(r.Target.Root, r.Prefix, r.Headers.Response)
	}
	return r.buildProxy(transport)
}

func (r *Route) buildProxy(transport *http.Transport) *httputil.ReverseProxy {
	proxy := &httputil.ReverseProxy{
		Rewrite:      r.rewriteRequest,
		Transport:    transport,
		ErrorHandler: UpstreamErrorHandler,
	}
	if len(r.Headers.Response) > 0 {
		proxy.ModifyResponse = r.Headers.rewriteResponse
	}
	return proxy
}

// rewriteRequest 换上游 scheme/host 并按配置剥掉路径前缀。SetURL 会把上游基路径
// 与完整入站路径拼接，剥前缀后必须重写 Path；RawPath 同步重建，
// 保证 %2F 等编码语义原样转发到上游而非被重新转义。
func (r *Route) rewriteRequest(request *httputil.ProxyRequest) {
	request.SetURL(r.Target.URL)
	restPath := strings.TrimPrefix(request.In.URL.Path, r.Prefix)
	restRawPath := trimEncodedPrefix(request.In.URL.EscapedPath(), len(request.In.URL.Path)-len(restPath))
	request.Out.URL.Path, request.Out.URL.RawPath = joinOutPath(r.Target.URL, restPath, restRawPath)
	if r.Host != "" {
		request.Out.Host = r.Host
	}
	r.Headers.rewriteRequest(request.Out.Header)
}

// trimEncodedPrefix 在原始编码路径上剥掉指定字节数的解码前缀：EscapedPath 的
// 每一段（%XX 或单字节）恰好解码为 1 字节，与 Path 字节序列一一对应，
// 按字节数逐段消费即可，避免把 %2F 重新转义成 %252F。
func trimEncodedPrefix(escaped string, skip int) string {
	for ; skip > 0 && escaped != ""; skip-- {
		if escaped[0] == '%' && len(escaped) >= 3 {
			escaped = escaped[3:]
			continue
		}
		escaped = escaped[1:]
	}
	return escaped
}

// joinOutPath 按斜杠边界拼接上游基路径与入站剩余路径，语义与 httputil 内部
// joinURLPath 一致：Path 与 RawPath（完整路径编码）按同一斜杠规则同步拼接，
// RawPath 的 base 段取 base 的完整转义、rest 段取传入的编码剩余路径；
// 结果无编码差异时 RawPath 置空，回归默认转义。
func joinOutPath(base *url.URL, restPath, restRawPath string) (string, string) {
	baseRaw := base.EscapedPath()
	restRaw := restRawPath
	if restRaw == "" {
		restRaw = (&url.URL{Path: restPath}).EscapedPath()
	}
	baseSlash := strings.HasSuffix(base.Path, "/")
	restSlash := strings.HasPrefix(restPath, "/")
	path := singleJoiningSlash(base.Path, restPath)
	raw := baseRaw + restRaw
	switch {
	case baseSlash && restSlash:
		raw = baseRaw + restRaw[1:]
	case !baseSlash && !restSlash:
		raw = baseRaw + "/" + restRaw
	}
	if raw == (&url.URL{Path: path}).EscapedPath() {
		raw = ""
	}
	return path, raw
}

// singleJoiningSlash 拼接两段路径，避免出现双斜杠或丢斜杠。
func singleJoiningSlash(base, rest string) string {
	baseSlash := strings.HasSuffix(base, "/")
	restSlash := strings.HasPrefix(rest, "/")
	switch {
	case baseSlash && restSlash:
		return base + rest[1:]
	case !baseSlash && !restSlash:
		return base + "/" + rest
	}
	return base + rest
}

// UpstreamErrorHandler 上游不可达时回 502，不向客户端透出传输层错误细节。
// 供本包的路由代理与 proxy 包的透传代理共用。
func UpstreamErrorHandler(writer http.ResponseWriter, request *http.Request, err error) {
	log.Error("upstream request failed host=%s path=%s: %v", request.Host, request.URL.Path, err)
	writer.WriteHeader(http.StatusBadGateway)
	_, _ = io.WriteString(writer, "502 Bad Gateway")
}

func (r *Route) fingerprint() string {
	return strings.Join([]string{
		r.Prefix,
		r.Target.Summary,
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

type staticHandler struct {
	root            http.Dir
	prefix          string
	responseHeaders map[string]string
}

func newStaticHandler(root, prefix string, responseHeaders map[string]string) *staticHandler {
	return &staticHandler{root: http.Dir(root), prefix: prefix, responseHeaders: responseHeaders}
}

// ServeHTTP 以本地目录为根托起静态文件：目录命中时回退 index.html，
// 不生成目录列表；路径解析交由 http.Dir 以阻断路径穿越。
func (h *staticHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	for name, value := range h.responseHeaders {
		writer.Header().Set(name, value)
	}
	relPath := strings.TrimPrefix(strings.TrimPrefix(request.URL.Path, h.prefix), "/")
	file, err := h.root.Open(relPath)
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	if !info.IsDir() {
		http.ServeContent(writer, request, info.Name(), info.ModTime(), file)
		return
	}
	index, err := h.root.Open(relPath + "/index.html")
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	defer index.Close()
	indexInfo, err := index.Stat()
	if err != nil || indexInfo.IsDir() {
		http.NotFound(writer, request)
		return
	}
	http.ServeContent(writer, request, indexInfo.Name(), indexInfo.ModTime(), index)
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
