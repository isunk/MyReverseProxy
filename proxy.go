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
	"slices"
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
		passthrough: newPassthroughProxy(transport),
	}
	if err := p.reload(); err != nil {
		return nil, err
	}
	return p, nil
}

// newPassthroughProxy 未命中路由时透传到原始目标，沿用入站 scheme 与 Host。
func newPassthroughProxy(transport *http.Transport) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			scheme := "http"
			if request.In.TLS != nil {
				scheme = "https"
			}
			request.SetURL(&url.URL{Scheme: scheme, Host: request.In.Host})
			request.Out.Host = request.In.Host
		},
		Transport:    transport,
		ErrorHandler: upstreamErrorHandler,
	}
}

func (p *proxy) reload() error {
	table, nameservers, err := loadTable(p.configPath)
	if err != nil {
		return err
	}
	current := p.table.Load()
	routesChanged := current == nil || current.fingerprint() != table.fingerprint()
	previous := p.nameservers.serverAddresses()
	if err := p.nameservers.update(nameservers); err != nil {
		return err
	}
	nameserversChanged := !slices.Equal(previous, p.nameservers.serverAddresses())
	if !routesChanged && !nameserversChanged {
		logDebugf("config parsed but routes and nameservers unchanged, keeping current pool")
		return nil
	}
	if routesChanged {
		table.installHandlers(p.transport)
		p.table.Store(table)
	}
	p.resetCaches()
	return nil
}

// resetCaches 丢弃与旧配置绑定的派生状态：空闲上游连接、已解析地址、已签发证书。
func (p *proxy) resetCaches() {
	p.transport.CloseIdleConnections()
	p.nameservers.clearCache()
	if p.authority != nil {
		p.authority.clearCache()
	}
}

// routeHandler 选出本次请求的处理器：命中路由走该路由，否则透传原始目标。
func (p *proxy) routeHandler(request *http.Request) (http.Handler, string) {
	domain := domainOf(request)
	route, matched := p.table.Load().pick(domain, request.URL.Path)
	if !matched {
		logDebugf("no route matched, passing through to original target domain=%s", domain)
		return p.passthrough, domain
	}
	logDebugf("route matched domain=%s prefix=%s upstream=%s", domain, route.prefix, route.target.summary)
	return route.handler, domain
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

func (p *proxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodConnect {
		p.handleConnect(writer, request)
		return
	}
	start := time.Now()
	recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
	handler, domain := p.routeHandler(request)
	defer func() {
		logInfof("request domain=%s path=%s status=%d elapsed=%s", domain, request.URL.Path, recorder.status, time.Since(start))
	}()
	handler.ServeHTTP(recorder, request)
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
