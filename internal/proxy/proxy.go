package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"mrp/internal/ca"
	"mrp/internal/config"
	"mrp/internal/dns"
	"mrp/internal/frontend"
	"mrp/internal/log"
	"mrp/internal/route"
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

	// reloadMu 串行化 reload：WatchFile 轮询与 SIGHUP 可并发触发，不加锁时两个 reload 的 Load→Store 交错会让旧配置覆盖新配置
	reloadMu sync.Mutex
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
			request.Out.URL = passthroughURL(request.In)
			request.Out.Host = request.In.Host
		},
		Transport:    transport,
		ErrorHandler: route.UpstreamErrorHandler,
	}
}

// passthroughURL 给出未命中路由时的转发目标：完整 URL 原样转发，隧道内层收到的源形式请求按连接协议与 Host 头补齐 scheme 与端口。
func passthroughURL(inbound *http.Request) *url.URL {
	if inbound.URL.Scheme != "" {
		outbound := *inbound.URL
		return &outbound
	}
	protocol := "http"
	if inbound.TLS != nil {
		protocol = "https"
	}
	outbound := *inbound.URL
	parsed, err := url.Parse(protocol + "://" + inbound.Host + inbound.URL.RequestURI())
	if err == nil {
		outbound = *parsed
	}
	return &outbound
}

// reload 重载配置：路由与 DNS 服务器无变化时保留现状，任一项变化则替换并清理派生状态；加锁串行化保证读文件、比指纹、换表、清缓存的顺序一致。
func (p *Proxy) reload() error {
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()
	table, nameservers, err := config.Load(p.configPath)
	if err != nil {
		return err
	}
	current := p.table.Load()
	routesChanged := current == nil || current.Fingerprint() != table.Fingerprint()
	nameserversChanged, err := p.nameservers.Update(nameservers)
	if err != nil {
		return err
	}
	if !routesChanged && !nameserversChanged {
		log.Debug("config unchanged")
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

// routeHandler 选出本次请求的处理器：命中路由走该路由，否则透传原始目标；Debug 级日志一行呈现入站 URL 与改写后实际发出的 URL。
func (p *Proxy) routeHandler(request *http.Request, protocol string, port int, domain string) http.Handler {
	targetRoute, matched := p.table.Load().Pick(protocol, port, domain, request.URL.Path)
	inbound := absoluteTarget(protocol, port, domain, request.URL)
	if !matched {
		log.Debug("route %s -> passthrough", inbound)
		return p.passthrough
	}
	if targetRoute.Status != 0 {
		log.Debug("route %s -> status %d", inbound, targetRoute.Status)
		return targetRoute.Handler()
	}
	if targetRoute.Target.Root != "" {
		log.Debug("route %s -> %s", inbound, targetRoute.Target.Summary)
		return targetRoute.Handler()
	}
	log.Debug("route %s -> %s", inbound, targetRoute.OutboundURL(request.URL, request.URL.RawQuery).String())
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
	recorder := &frontend.StatusRecorder{ResponseWriter: writer, Status: http.StatusOK}
	handler := p.routeHandler(request, protocol, port, domain)
	defer func() {
		log.Info("request domain=%s protocol=%s port=%d method=%s path=%s status=%d elapsed=%s", domain, protocol, port, request.Method, request.URL.Path, recorder.Status, time.Since(start))
	}()
	handler.ServeHTTP(recorder, request)
}

// WatchFile 轮询配置文件元数据，size 与 mtime 都未变化时跳过整份读取，内容变化才触发重载，失败保留旧配置；stop 为 nil 时常驻运行。
func (p *Proxy) WatchFile(interval time.Duration, stop <-chan struct{}) {
	var previousSize int64
	var previousModTime time.Time
	previousKnown := false
	if info, err := os.Stat(p.configPath); err == nil {
		previousSize, previousModTime, previousKnown = info.Size(), info.ModTime(), true
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	statFailed := false
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			info, err := os.Stat(p.configPath)
			if err != nil {
				// 文件缺失（被删除）期间只提示一次，避免每轮轮询刷屏；恢复后照常比对重载
				if !statFailed {
					statFailed = true
					log.Warn("stat config failed config=%s: %v", p.configPath, err)
				}
				continue
			}
			statFailed = false
			// 比 size 与 mtime 而非读全文；mtime 为毫秒级粒度，间隔极短的连续保存可能只触发一次重载
			if previousKnown && info.Size() == previousSize && info.ModTime().Equal(previousModTime) {
				continue
			}
			previousSize, previousModTime, previousKnown = info.Size(), info.ModTime(), true
			if err := p.reload(); err != nil {
				log.Error("reload failed, keeping previous config: %v", err)
				continue
			}
			log.Info("config reloaded")
		}
	}
}

// Reload 对外暴露的配置重载，供信号处理触发。
func (p *Proxy) Reload() error {
	return p.reload()
}

// NewTransport 构建上游连接池：关闭环境代理防回环、放宽同主机连接复用，并开启 TLS 会话复用，避免每条新上游连接重跑完整握手。
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
