package main

import (
	"io"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
)

// headerRewrite 转发时改写的消息头：request 写入发往上游的请求头，
// response 覆盖下游返回的响应头。Set 语义，覆盖同名已有值。
type headerRewrite struct {
	request  map[string]string
	response map[string]string
}

func (h *headerRewrite) rewriteRequest(headers http.Header) {
	for name, value := range h.request {
		headers.Set(name, value)
	}
}

// rewriteResponse 作为 ReverseProxy 的 ModifyResponse 钩子；
// 没有响应头改写时构建器不挂载该钩子，避免每次响应空跑。
func (h *headerRewrite) rewriteResponse(response *http.Response) error {
	for name, value := range h.response {
		response.Header.Set(name, value)
	}
	return nil
}

// target 转发目标：远程上游 URI 与本地目录路径互斥。
// summary 预计算目标描述，日志直接打印，避免请求热路径重复格式化。
type target struct {
	url     *url.URL
	root    string
	summary string
}

func (t *target) local() bool { return t.root != "" }

// route 一条路径前缀到转发目标的映射。handler 在加载配置时构建完毕，
// 运行期只做前缀匹配，不再区分远程上游与本地目录。
type route struct {
	prefix  string
	host    string
	headers headerRewrite
	target  target
	handler http.Handler
}

func (r *route) buildHandler(transport *http.Transport) http.Handler {
	if r.target.local() {
		return newStaticHandler(r.target.root, r.prefix, r.headers.response)
	}
	return r.buildProxy(transport)
}

func (r *route) buildProxy(transport *http.Transport) *httputil.ReverseProxy {
	proxy := &httputil.ReverseProxy{
		Rewrite:      r.rewriteRequest,
		Transport:    transport,
		ErrorHandler: upstreamErrorHandler,
	}
	if len(r.headers.response) > 0 {
		proxy.ModifyResponse = r.headers.rewriteResponse
	}
	return proxy
}

// rewriteRequest 换上游 scheme/host 并按配置剥掉路径前缀。SetURL 会把上游基路径
// 与完整入站路径拼接，剥前缀后必须重写 Path，故此处显式覆盖。
func (r *route) rewriteRequest(request *httputil.ProxyRequest) {
	request.SetURL(r.target.url)
	request.Out.URL.Path = joinPath(r.target.url.Path, strings.TrimPrefix(request.In.URL.Path, r.prefix))
	request.Out.URL.RawPath = ""
	if r.host != "" {
		request.Out.Host = r.host
	}
	r.headers.rewriteRequest(request.Out.Header)
}

// upstreamErrorHandler 上游不可达时回 502，不向客户端透出传输层错误细节。
func upstreamErrorHandler(writer http.ResponseWriter, request *http.Request, err error) {
	logErrorf("upstream request failed host=%s path=%s: %v", request.Host, request.URL.Path, err)
	writer.WriteHeader(http.StatusBadGateway)
	_, _ = io.WriteString(writer, "502 Bad Gateway")
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

// routeTable 按域名索引的路由表。表在加载配置时一次性构建完毕，
// 运行期只读，热加载以整体替换实现。
type routeTable struct {
	byDomain map[string][]*route
}

func (t *routeTable) has(domain string) bool {
	_, ok := t.byDomain[domain]
	return ok
}

// pick 在该域名的路由中按最长前缀匹配，结果与条目顺序无关。
func (t *routeTable) pick(domain, path string) (*route, bool) {
	var best *route
	for _, entry := range t.byDomain[domain] {
		if pathHasPrefix(path, entry.prefix) && (best == nil || len(entry.prefix) > len(best.prefix)) {
			best = entry
		}
	}
	return best, best != nil
}

// installHandlers 为每条路由构建处理器，构建完成后路由表内容不再变化。
func (t *routeTable) installHandlers(transport *http.Transport) {
	for _, entries := range t.byDomain {
		for _, entry := range entries {
			entry.handler = entry.buildHandler(transport)
		}
	}
}

// fingerprint 生成路由表的稳定指纹，用于判断热加载后的配置是否真正变化：
// 无变化时跳过重建处理器与拆掉存量连接。
func (t *routeTable) fingerprint() string {
	domains := slices.Sorted(maps.Keys(t.byDomain))
	var buffer strings.Builder
	for _, domain := range domains {
		buffer.WriteString(domain)
		buffer.WriteByte('\x00')
		for _, entry := range t.byDomain[domain] {
			buffer.WriteString(entry.fingerprint())
		}
	}
	return buffer.String()
}

func (r *route) fingerprint() string {
	return strings.Join([]string{
		r.prefix,
		r.target.summary,
		r.host,
		headerFingerprint(r.headers.request),
		headerFingerprint(r.headers.response),
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

// joinPath 拼接上游基路径与入站剩余路径，处理两端斜杠，避免出现双斜杠或丢斜杠。
func joinPath(base, rest string) string {
	if rest == "" {
		rest = "/"
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	if base == "" {
		return rest
	}
	return strings.TrimSuffix(base, "/") + rest
}
