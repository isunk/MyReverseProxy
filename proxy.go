package main

import (
	"bytes"
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

type proxy struct {
	configPath  string
	table       atomic.Pointer[routeTable]
	transport   *http.Transport
	tlsConfig   *tls.Config
	passthrough *httputil.ReverseProxy
}

func newProxy(configPath string, transport *http.Transport, tlsConfig *tls.Config) (*proxy, error) {
	p := &proxy{
		configPath: configPath,
		transport:  transport,
		tlsConfig:  tlsConfig,
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
	table, err := loadTable(p.configPath)
	if err != nil {
		return err
	}
	for _, entries := range table.byDomain {
		for _, entry := range entries {
			entry.proxy = p.newRouteProxy(entry)
		}
	}
	p.table.Store(table)
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
		logDebugf("route matched domain=%s prefix=%s upstream=%s", domain, entry.prefix, entry.target)
		entry.proxy.ServeHTTP(recorder, request)
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
	client, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	return client, true
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
	upstream, err := net.DialTimeout("tcp", target, tunnelDialTimeout)
	if err != nil {
		logErrorf("tunnel target connection failed target=%s: %v", target, err)
		_, _ = client.Write([]byte(connectBadGateway))
		return
	}
	defer upstream.Close()
	if _, err := client.Write([]byte(connectEstablished)); err != nil {
		return
	}
	go func() {
		io.Copy(upstream, client)
		upstream.Close()
	}()
	io.Copy(client, upstream)
}
