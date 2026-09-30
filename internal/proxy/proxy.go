package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"mrp/internal/ca"
	"mrp/internal/config"
	"mrp/internal/dns"
	"mrp/internal/logging"
	"mrp/internal/route"
	"mrp/internal/server"
)

// 请求协议取值与省略端口时补齐的默认端口
const (
	protocolHTTP     = "http"
	protocolHTTPS    = "https"
	defaultHTTPPort  = 80
	defaultHTTPSPort = 443
)

const (
	responseHeaderTimeout  = 30 * time.Second
	maxIdleConnections     = 256
	maxIdleConnectionsHost = 64
	transportBufferSize    = 32 << 10
	maxCachedTLSSessions   = 64
)

// Proxy 持有当前生效的路由表与 DNS 解析器，路由表整体替换以支持热加载。
type Proxy struct {
	configPath  string
	table       atomic.Pointer[route.Table]
	transport   *http.Transport
	tlsConfig   *tls.Config
	authority   *ca.Authority
	passthrough *httputil.ReverseProxy
	nameservers *dns.Resolver
}

func New(configPath string, transport *http.Transport, tlsConfig *tls.Config, authority *ca.Authority, nameservers *dns.Resolver) (*Proxy, error) {
	p := &Proxy{
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
		ErrorHandler: route.UpstreamErrorHandler,
	}
}

// reload 重载配置：路由与 DNS 服务器无变化时保留现状，任一项变化则替换并清理派生状态。
func (p *Proxy) reload() error {
	table, nameservers, err := config.Load(p.configPath)
	if err != nil {
		return err
	}
	current := p.table.Load()
	routesChanged := current == nil || current.Fingerprint() != table.Fingerprint()
	previous := p.nameservers.Addresses()
	if err := p.nameservers.Update(nameservers); err != nil {
		return err
	}
	nameserversChanged := !slices.Equal(previous, p.nameservers.Addresses())
	if !routesChanged && !nameserversChanged {
		logging.Debugf("config parsed but routes and nameservers unchanged, keeping current pool")
		return nil
	}
	if routesChanged {
		table.InstallHandlers(p.transport)
		p.table.Store(table)
	}
	p.resetCaches()
	return nil
}

// resetCaches 丢弃与旧配置绑定的派生状态：空闲上游连接、已解析地址、已签发证书。
func (p *Proxy) resetCaches() {
	p.transport.CloseIdleConnections()
	p.nameservers.ClearCache()
	if p.authority != nil {
		p.authority.ClearCache()
	}
}

// Handler 返回代理的统一处理器：单端口监听与 MITM 隧道内层服务共用。
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		p.serveRequest(writer, request)
	})
}

// targetOf 从请求提取转发目标的协议、端口与域名：CONNECT 按 https 处理；
// 代理绝对形式请求取 URL 的 scheme 与目标主机；隧道内层收到的源形式请求
// 取 Host 头并按连接是否为 TLS 判定协议。省略端口时按协议补默认端口。
func targetOf(request *http.Request) (string, int, string) {
	if request.Method == http.MethodConnect {
		host, port := hostAndPort(request.Host, defaultHTTPSPort)
		return protocolHTTPS, port, strings.ToLower(host)
	}
	if request.URL.Scheme == "" {
		protocol := protocolHTTP
		if request.TLS != nil {
			protocol = protocolHTTPS
		}
		host, port := hostAndPort(request.Host, defaultPort(protocol))
		return protocol, port, strings.ToLower(host)
	}
	return request.URL.Scheme, effectivePort(request.URL), hostOnly(request.URL.Host)
}

// hostAndPort 拆分 Host 为名称与端口，端口缺失或非法时补 fallback。
func hostAndPort(host string, fallback int) (string, int) {
	name, port, err := net.SplitHostPort(host)
	if err != nil || name == "" {
		name = host
	}
	if number, err := strconv.Atoi(port); err == nil && number >= 1 && number <= 65535 {
		return name, number
	}
	return name, fallback
}

// hostOnly 取 Host 头的主机名部分并小写化，便于与路由表键对齐。
func hostOnly(host string) string {
	if host, _, err := net.SplitHostPort(host); err == nil {
		return strings.ToLower(host)
	}
	return strings.ToLower(host)
}

// defaultPort 按协议补齐省略的目标端口
func defaultPort(protocol string) int {
	if protocol == protocolHTTPS {
		return defaultHTTPSPort
	}
	return defaultHTTPPort
}

// effectivePort 取目标主机端口，省略时按协议补默认端口。
func effectivePort(targetURL *url.URL) int {
	if port, err := strconv.Atoi(targetURL.Port()); err == nil {
		return port
	}
	return defaultPort(targetURL.Scheme)
}

// routeHandler 选出本次请求的处理器：命中路由走该路由，否则透传原始目标。
func (p *Proxy) routeHandler(request *http.Request, protocol string, port int, domain string) http.Handler {
	targetRoute, matched := p.table.Load().Pick(protocol, port, domain, request.URL.Path)
	if !matched {
		logging.Debugf("no route matched, passing through to original target domain=%s protocol=%s port=%d", domain, protocol, port)
		return p.passthrough
	}
	logging.Debugf("route matched domain=%s protocol=%s port=%d prefix=%s upstream=%s", domain, protocol, port, targetRoute.Prefix, targetRoute.Target.Summary)
	return targetRoute.Handler()
}

// serveRequest 是单条入站请求的入口：CONNECT 走隧道，其余按目标协议、端口与域名路由或透传
func (p *Proxy) serveRequest(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodConnect {
		p.handleConnect(writer, request)
		return
	}
	protocol, port, domain := targetOf(request)
	start := time.Now()
	recorder := &server.StatusRecorder{ResponseWriter: writer, Status: http.StatusOK}
	handler := p.routeHandler(request, protocol, port, domain)
	defer func() {
		logging.Infof("request domain=%s protocol=%s port=%d path=%s status=%d elapsed=%s", domain, protocol, port, request.URL.Path, recorder.Status, time.Since(start))
	}()
	handler.ServeHTTP(recorder, request)
}

// WatchFile 轮询配置文件内容，变化即触发重载，失败保留旧配置。stop 为 nil 时常驻运行。
func (p *Proxy) WatchFile(interval time.Duration, stop <-chan struct{}) {
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
				logging.Warnf("failed to read routing config: %v", err)
				continue
			}
			if bytes.Equal(data, previous) {
				continue
			}
			previous = data
			if err := p.reload(); err != nil {
				logging.Errorf("config reload failed, keeping previous config: %v", err)
				continue
			}
			logging.Infof("config changed, hot reloaded")
		}
	}
}

// Reload 对外暴露的配置重载，供信号处理触发。
func (p *Proxy) Reload() error {
	return p.reload()
}

// NewTransport 构建上游连接池：关闭环境代理防回环、放宽同主机连接复用，
// 并开启 TLS 会话复用，避免每条新上游连接重跑完整握手。
func NewTransport(dialContext func(context.Context, string, string) (net.Conn, error)) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // 禁用环境代理，避免代理流量经上游代理回环到自身
	transport.DialContext = dialContext
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	// 默认 MaxIdleConnsPerHost=2，代理到同一上游的并发请求会频繁重建连接（TCP+TLS 握手）
	transport.MaxIdleConns = maxIdleConnections
	transport.MaxIdleConnsPerHost = maxIdleConnectionsHost
	transport.ReadBufferSize = transportBufferSize
	transport.WriteBufferSize = transportBufferSize
	// ClientSessionCache 为 nil 时 Go 禁用会话复用，每次新上游连接都要走完整 TLS 握手
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true, // mrp 位于设备与上游之间，上游证书校验交由设备端完成
		ClientSessionCache: tls.NewLRUClientSessionCache(maxCachedTLSSessions),
	}
	return transport
}
