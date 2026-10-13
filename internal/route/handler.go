package route

import (
	"io"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"

	"mrp/internal/log"
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

func (r *Route) buildHandler(transport *http.Transport) http.Handler {
	if r.Status != 0 {
		return newStatusHandler(r.Status, r.Body, r.Headers.Response)
	}
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

// UpstreamErrorHandler 上游不可达时回 502，不向客户端透出传输层错误细节。
// 供本包的路由代理与 proxy 包的透传代理共用。
func UpstreamErrorHandler(writer http.ResponseWriter, request *http.Request, err error) {
	log.Error("upstream request failed host=%s path=%s: %v", request.Host, request.URL.Path, err)
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

type statusHandler struct {
	status          int
	body            string
	responseHeaders map[string]string
}

func newStatusHandler(status int, body string, responseHeaders map[string]string) *statusHandler {
	return &statusHandler{status: status, body: body, responseHeaders: responseHeaders}
}

func (h *statusHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	for name, value := range h.responseHeaders {
		writer.Header().Set(name, value)
	}
	if h.body != "" {
		if writer.Header().Get("Content-Type") == "" {
			writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		writer.Header().Set("Content-Length", strconv.Itoa(len(h.body)))
	}
	writer.WriteHeader(h.status)
	if request.Method != http.MethodHead && h.body != "" {
		_, _ = io.WriteString(writer, h.body)
	}
}
