package route

import (
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"mrp/internal/log"
)

// HeaderRewrite 转发时改写的消息头：Request 写入发往上游，Response 覆盖下游返回，Set 语义覆盖同名。
type HeaderRewrite struct {
	Request  map[string]string
	Response map[string]string
}

// setHeaders 按配置 Set 消息头，同名覆盖。
func setHeaders(header http.Header, values map[string]string) {
	for name, value := range values {
		header.Set(name, value)
	}
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
	// 没有响应头改写时不挂载钩子，避免每次响应空跑
	if len(r.Headers.Response) > 0 {
		proxy.ModifyResponse = func(response *http.Response) error {
			setHeaders(response.Header, r.Headers.Response)
			return nil
		}
	}
	return proxy
}

// OutboundURL 给出入站请求改写后发往上游的完整 URL：换 scheme/host、剥前缀拼接上游基路径，转发与路由日志共用保证一致。
func (r *Route) OutboundURL(inboundURL *url.URL) *url.URL {
	base := r.Target.URL
	outbound := *inboundURL
	outbound.Scheme = base.Scheme
	outbound.Host = base.Host
	if base.RawQuery == "" || inboundURL.RawQuery == "" {
		outbound.RawQuery = base.RawQuery + inboundURL.RawQuery
	} else {
		outbound.RawQuery = base.RawQuery + "&" + inboundURL.RawQuery
	}
	outbound.Path, outbound.RawPath = buildOutPath(inboundURL, r.Prefix, base)
	return &outbound
}

// rewriteRequest 换上游 scheme/host 并按配置剥前缀，RawPath 同步重建保证 %2F 等编码原样转发。
func (r *Route) rewriteRequest(request *httputil.ProxyRequest) {
	request.Out.URL = r.OutboundURL(request.In.URL)
	if r.Host != "" {
		request.Out.Host = r.Host
	}
	setHeaders(request.Out.Header, r.Headers.Request)
}

// UpstreamErrorHandler 上游不可达时回 502，不向客户端透出传输层错误；按实际拨号目标记录，供本包路由代理与 proxy 包透传代理共用。
func UpstreamErrorHandler(writer http.ResponseWriter, request *http.Request, err error) {
	log.Error("upstream failed upstream=%s err=%v", request.URL.String(), err)
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

// ServeHTTP 以本地目录为根托管静态文件：目录命中回退 index.html，不生成目录列表，路径解析交给 http.Dir 阻断穿越。
func (h *staticHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	setHeaders(writer.Header(), h.responseHeaders)
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
	setHeaders(writer.Header(), h.responseHeaders)
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
