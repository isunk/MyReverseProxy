package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const (
	connectEstablished = "HTTP/1.1 200 Connection Established\r\n\r\n"
	connectBadGateway  = "HTTP/1.1 502 Bad Gateway\r\n\r\n"
	tunnelDialTimeout  = 10 * time.Second
)

// proxy 持有当前生效的路由表与 DNS 解析器，路由表整体替换以支持热加载。
type proxy struct {
	configPath  string
	table       atomic.Pointer[routeTable]
	transport   *http.Transport
	tlsConfig   *tls.Config
	authority   *certificateAuthority
	passthrough *httputil.ReverseProxy
	nameservers *nameserverSet
}

func newProxy(configPath string, transport *http.Transport, tlsConfig *tls.Config, authority *certificateAuthority, nameservers *nameserverSet) (*proxy, error) {
	p := &proxy{
		configPath:  configPath,
		transport:   transport,
		tlsConfig:   tlsConfig,
		authority:   authority,
		nameservers: nameservers,
	}
	p.passthrough = &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			scheme := "http"
			if request.In.TLS != nil {
				scheme = "https"
			}
			request.SetURL(&url.URL{Scheme: scheme, Host: request.In.Host})
			request.Out.Host = request.In.Host
		},
		Transport:    transport,
		ErrorHandler: p.errorHandler,
	}
	if err := p.reload(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *proxy) reload() error {
	table, nameservers, err := loadTable(p.configPath)
	if err != nil {
		return err
	}
	if err := p.nameservers.update(nameservers); err != nil {
		return err
	}
	for _, entries := range table.byDomain {
		for _, entry := range entries {
			if entry.fileRoot != "" {
				entry.fileServer = newStaticHandler(entry.fileRoot, entry.prefix, entry.responseHeaders)
			} else {
				entry.proxy = p.newRouteProxy(entry)
			}
		}
	}
	p.table.Store(table)
	// 丢弃旧路由的存量派生缓存：空闲上游连接与已签发证书，避免残留旧目标
	p.transport.CloseIdleConnections()
	if p.authority != nil {
		p.authority.clearCache()
	}
	return nil
}

func (p *proxy) watchFile(interval time.Duration, stop <-chan struct{}) {
	var previous []byte
	if data, err := os.ReadFile(p.configPath); err == nil {
		previous = data
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			data, err := os.ReadFile(p.configPath)
			if err != nil {
				logWarnf("failed to read routing config: %v", err)
				continue
			}
			if bytes.Equal(data, previous) {
				continue
			}
			previous = data
			if err := p.reload(); err != nil {
				logErrorf("config reload failed, keeping previous config: %v", err)
				continue
			}
			logInfof("config changed, hot reloaded")
		}
	}
}

func (p *proxy) newRouteProxy(entry *route) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(entry.target)
			request.Out.URL.Path = joinPath(entry.target.Path, strings.TrimPrefix(request.In.URL.Path, entry.prefix))
			request.Out.URL.RawPath = ""
			if entry.host != "" {
				request.Out.Host = entry.host
			}
			for name, value := range entry.requestHeaders {
				request.Out.Header.Set(name, value)
			}
		},
		Transport:      p.transport,
		ErrorHandler:   p.errorHandler,
		ModifyResponse: applyResponseHeaders(entry.responseHeaders),
	}
}

// applyResponseHeaders 返回覆盖响应头的 ModifyResponse 钩子；无配置时返回 nil 以跳过
func applyResponseHeaders(headers map[string]string) func(*http.Response) error {
	if len(headers) == 0 {
		return nil
	}
	return func(response *http.Response) error {
		for name, value := range headers {
			response.Header.Set(name, value)
		}
		return nil
	}
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

func (p *proxy) errorHandler(writer http.ResponseWriter, request *http.Request, err error) {
	logErrorf("upstream request failed host=%s path=%s: %v", request.Host, request.URL.Path, err)
	writer.WriteHeader(http.StatusBadGateway)
	_, _ = io.WriteString(writer, "502 Bad Gateway")
}

func (p *proxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodConnect {
		p.handleConnect(writer, request)
		return
	}
	domain := domainOf(request)
	entry, matched := p.table.Load().pick(domain, request.URL.Path)
	start := time.Now()
	recorder := &logWriter{ResponseWriter: writer, status: http.StatusOK}
	defer func() {
		logInfof("request domain=%s path=%s status=%d elapsed=%s", domain, request.URL.Path, recorder.status, time.Since(start))
	}()
	if matched {
		logDebugf("route matched domain=%s prefix=%s upstream=%s", domain, entry.prefix, entry.upstream())
		if entry.fileServer != nil {
			entry.fileServer.ServeHTTP(recorder, request)
		} else {
			entry.proxy.ServeHTTP(recorder, request)
		}
		return
	}
	logDebugf("no route matched, passing through to original target domain=%s", domain)
	p.passthrough.ServeHTTP(recorder, request)
}

func (p *proxy) handleConnect(writer http.ResponseWriter, request *http.Request) {
	client, ok := hijackConn(writer)
	if !ok {
		return
	}
	// serveConnect 返回 false 表示连接仍在手里（隧道结束、502 或握手失败），由这里收尾
	if !p.serveConnect(client, request) {
		client.Close()
	}
}

func hijackConn(writer http.ResponseWriter) (net.Conn, bool) {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		http.Error(writer, "hijack unsupported", http.StatusInternalServerError)
		return nil, false
	}
	client, bufioRW, err := hijacker.Hijack()
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	// 保留 http.Server 内部 bufio.Reader 预读的数据（客户端紧跟 CONNECT 发送的字节）
	return &bufferedConn{Conn: client, reader: bufioRW.Reader}, true
}

// serveConnect 按 CONNECT 目标域名分发：命中路由走 MITM，否则透传隧道；
// 返回 true 表示连接已移交内层 HTTP 服务（hijack 链），调用方不得再关闭
func (p *proxy) serveConnect(client net.Conn, request *http.Request) bool {
	domain := hostOnly(request.Host)
	if !p.table.Load().has(domain) {
		logInfof("connect target=%s mode=tunnel", request.Host)
		p.tunnel(client, request.Host)
		return false
	}
	if p.tlsConfig == nil {
		_, _ = client.Write([]byte(connectBadGateway))
		return false
	}
	logInfof("connect domain=%s mode=mitm", domain)
	if _, err := client.Write([]byte(connectEstablished)); err != nil {
		return false
	}
	return serveSingleConn(newHTTPServer(p), tls.Server(client, p.tlsConfig))
}

func (p *proxy) tunnel(client net.Conn, target string) {
	ctx, cancel := context.WithTimeout(context.Background(), tunnelDialTimeout)
	defer cancel()
	upstream, err := p.nameservers.DialContext(ctx, "tcp", target)
	if err != nil {
		logErrorf("tunnel target connection failed target=%s: %v", target, err)
		_, _ = client.Write([]byte(connectBadGateway))
		return
	}
	defer upstream.Close()
	if _, err := client.Write([]byte(connectEstablished)); err != nil {
		return
	}
	// 双向拷贝：client→upstream 起协程，主协程跑 upstream→client。任一方向结束后
	// tunnel 返回，由调用方关闭 client，进而打断对端读取使协程退出。
	go func() {
		io.Copy(upstream, client)
		upstream.Close()
	}()
	io.Copy(client, upstream)
}
