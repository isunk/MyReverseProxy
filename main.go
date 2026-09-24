package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Servers []Server `yaml:"servers"`
}

type Server struct {
	Domain string  `yaml:"domain"`
	Routes []Route `yaml:"routes"`
}

type Route struct {
	Prefix    string `yaml:"prefix"`
	Upstream  string `yaml:"upstream"`
	Host      string `yaml:"host"`
	TLSVerify *bool  `yaml:"tls_verify"`
}

type route struct {
	prefix   string
	target   *url.URL
	host     string
	insecure bool
	proxy    *httputil.ReverseProxy
}

type routeTable struct {
	byDomain map[string][]*route
}

func (t *routeTable) pick(domain, path string) (*route, bool) {
	var best *route
	for _, r := range t.byDomain[domain] {
		if strings.HasPrefix(path, r.prefix) && (best == nil || len(r.prefix) > len(best.prefix)) {
			best = r
		}
	}
	return best, best != nil
}

func loadTable(path string) (*routeTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	t := &routeTable{byDomain: map[string][]*route{}}
	for _, s := range cfg.Servers {
		if s.Domain == "" {
			return nil, fmt.Errorf("domain 不能为空")
		}
		if _, ok := t.byDomain[s.Domain]; ok {
			return nil, fmt.Errorf("domain %q 重复定义", s.Domain)
		}
		seen := map[string]bool{}
		var rs []*route
		for _, r := range s.Routes {
			if r.Prefix == "" || r.Upstream == "" {
				return nil, fmt.Errorf("domain %q: prefix 与 upstream 均为必填", s.Domain)
			}
			if seen[r.Prefix] {
				return nil, fmt.Errorf("domain %q: prefix %q 重复定义", s.Domain, r.Prefix)
			}
			seen[r.Prefix] = true
			u, err := url.Parse(r.Upstream)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, fmt.Errorf("domain %q: 非法 upstream %q", s.Domain, r.Upstream)
			}
			rs = append(rs, &route{
				prefix:   r.Prefix,
				target:   u,
				host:     r.Host,
				insecure: r.TLSVerify != nil && !*r.TLSVerify,
			})
		}
		t.byDomain[s.Domain] = rs
	}
	return t, nil
}

type ctxKey int

const sniKey ctxKey = 1

type proxy struct {
	configPath  string
	table       atomic.Pointer[routeTable]
	trVerify    *http.Transport
	trInsecure  *http.Transport
	tlsConfig   *tls.Config
	passthrough *httputil.ReverseProxy
}

func newProxy(configPath string, trVerify, trInsecure *http.Transport, tlsCfg *tls.Config) (*proxy, error) {
	p := &proxy{
		configPath: configPath,
		trVerify:   trVerify,
		trInsecure: trInsecure,
		tlsConfig:  tlsCfg,
	}
	p.passthrough = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			scheme := "http"
			if pr.In.TLS != nil {
				scheme = "https"
			}
			pr.SetURL(&url.URL{Scheme: scheme, Host: pr.In.Host})
			pr.Out.Host = pr.In.Host
		},
		Transport:    trVerify,
		ErrorHandler: p.errorHandler,
	}
	if err := p.reload(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *proxy) reload() error {
	t, err := loadTable(p.configPath)
	if err != nil {
		return err
	}
	for _, rs := range t.byDomain {
		for _, r := range rs {
			tr := p.trVerify
			if r.insecure {
				tr = p.trInsecure
			}
			r.proxy = p.newRouteProxy(r, tr)
		}
	}
	p.table.Store(t)
	return nil
}

func (p *proxy) newRouteProxy(r *route, tr http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(r.target)
			pr.Out.URL.Path = joinPath(r.target.Path, strings.TrimPrefix(pr.In.URL.Path, r.prefix))
			pr.Out.URL.RawPath = ""
			if r.host != "" {
				pr.Out.Host = r.host
			}
		},
		Transport:    tr,
		ErrorHandler: p.errorHandler,
	}
}

func (p *proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("上游请求失败", "host", r.Host, "path", r.URL.Path, "error", err)
	w.WriteHeader(http.StatusBadGateway)
	io.WriteString(w, "502 Bad Gateway")
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	domain := domainOf(r)
	rt, matched := p.table.Load().pick(domain, r.URL.Path)
	start := time.Now()
	lw := &logWriter{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		slog.Info("request", "domain", domain, "path", r.URL.Path, "status", lw.status, "elapsed", time.Since(start))
	}()
	if matched {
		slog.Debug("路由命中", "domain", domain, "prefix", rt.prefix, "upstream", rt.target.String())
		rt.proxy.ServeHTTP(lw, r)
		return
	}
	slog.Debug("路由未命中，透传原始目标", "domain", domain)
	p.passthrough.ServeHTTP(lw, r)
}

func (p *proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer client.Close()

	domain := hostOnly(r.Host)
	_, matched := p.table.Load().pick(domain, "/")
	if !matched {
		slog.Info("connect", "target", r.Host, "mode", "tunnel")
		if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err == nil {
			p.tunnel(client, r.Host)
		}
		return
	}
	if p.tlsConfig == nil {
		client.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	slog.Info("connect", "domain", domain, "mode", "mitm")
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	cfg := p.tlsConfig.Clone()
	var sni string
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		sni = hello.ServerName
		return nil, nil
	}
	tc := tls.Server(client, cfg)
	srv := &http.Server{Handler: http.HandlerFunc(func(w2 http.ResponseWriter, r2 *http.Request) {
		if sni != "" {
			r2 = r2.WithContext(contextWithValue(r2, sni))
		}
		p.ServeHTTP(w2, r2)
	})}
	_ = srv.Serve(newOneConnListener(tc))
}

func (p *proxy) tunnel(client net.Conn, target string) {
	upstream, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		slog.Error("隧道目标连接失败", "target", target, "error", err)
		client.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer upstream.Close()
	go func() {
		io.Copy(upstream, client)
		upstream.Close()
	}()
	io.Copy(client, upstream)
}

func (p *proxy) serveHTTPS(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go p.serveTLSConn(c)
	}
}

func (p *proxy) serveTLSConn(c net.Conn) {
	defer c.Close()
	var sni string
	cfg := p.tlsConfig.Clone()
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		sni = hello.ServerName
		return nil, nil
	}
	tc := tls.Server(c, cfg)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sni != "" {
			r = r.WithContext(contextWithValue(r, sni))
		}
		p.ServeHTTP(w, r)
	})}
	_ = srv.Serve(newOneConnListener(tc))
}

type oneConnListener struct {
	conn net.Conn
}

func newOneConnListener(c net.Conn) *oneConnListener {
	return &oneConnListener{conn: c}
}

func (l *oneConnListener) Accept() (net.Conn, error) { return l.conn, nil }

func (l *oneConnListener) Close() error { return l.conn.Close() }

func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

func (p *proxy) run() {
	if httpListen != "" {
		go func() {
			slog.Info("HTTP 监听", "addr", httpListen)
			if err := http.ListenAndServe(httpListen, p); err != nil {
				fatal("HTTP 监听失败", err)
			}
		}()
	}
	if httpsListen != "" {
		if p.tlsConfig == nil {
			fatal("启用 HTTPS 监听时必须提供 --tls-cert 与 --tls-key", nil)
		}
		l, err := net.Listen("tcp", httpsListen)
		if err != nil {
			fatal("HTTPS 监听失败", err)
		}
		slog.Info("HTTPS 监听", "addr", httpsListen)
		go func() {
			if err := p.serveHTTPS(l); err != nil {
				fatal("HTTPS 服务退出", err)
			}
		}()
	}
	if httpListen == "" && httpsListen == "" {
		fatal("HTTP 与 HTTPS 监听均未启用", nil)
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for s := range sigCh {
		if s == syscall.SIGHUP {
			if err := p.reload(); err != nil {
				slog.Error("热加载失败，沿用当前配置", "error", err)
			} else {
				slog.Info("路由配置已热加载")
			}
			continue
		}
		return
	}
}

var (
	httpListen  string
	httpsListen string
)

func main() {
	configPath := flag.String("config", "routing.yaml", "路由配置文件路径")
	httpAddr := flag.String("http", ":80", "HTTP 监听地址，传空禁用")
	httpsAddr := flag.String("https", ":443", "HTTPS 监听地址，传空禁用")
	tlsCert := flag.String("tls-cert", "", "TLS 证书路径，启用 HTTPS 时必填")
	tlsKey := flag.String("tls-key", "", "TLS 私钥路径，启用 HTTPS 时必填")
	logLevel := flag.String("log-level", "info", "日志级别 debug/info/warn/error")
	flag.Parse()

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		fatal("非法日志级别 "+*logLevel, err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})))

	httpListen = *httpAddr
	httpsListen = *httpsAddr

	trVerify := http.DefaultTransport.(*http.Transport).Clone()
	trVerify.Proxy = nil
	trVerify.DialContext = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	trVerify.ResponseHeaderTimeout = 30 * time.Second
	trInsecure := trVerify.Clone()
	trInsecure.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	var tlsCfg *tls.Config
	if httpsListen != "" {
		if *tlsCert == "" || *tlsKey == "" {
			fatal("启用 HTTPS 监听时必须提供 --tls-cert 与 --tls-key", nil)
		}
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			fatal("加载 TLS 证书失败", err)
		}
		tlsCfg = &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"http/1.1"},
			MinVersion:   tls.VersionTLS12,
		}
	}

	p, err := newProxy(*configPath, trVerify, trInsecure, tlsCfg)
	if err != nil {
		fatal("加载路由配置失败", err)
	}
	p.run()
}

func domainOf(r *http.Request) string {
	if v, ok := r.Context().Value(sniKey).(string); ok && v != "" {
		return v
	}
	return hostOnly(r.Host)
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

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

type logWriter struct {
	http.ResponseWriter
	status int
}

func (l *logWriter) WriteHeader(code int) {
	l.status = code
	l.ResponseWriter.WriteHeader(code)
}

func (l *logWriter) Unwrap() http.ResponseWriter {
	return l.ResponseWriter
}

func contextWithValue(r *http.Request, sni string) context.Context {
	return context.WithValue(r.Context(), sniKey, sni)
}

func fatal(msg string, err error) {
	if err != nil {
		slog.Error(msg, "error", err)
	} else {
		slog.Error(msg)
	}
	os.Exit(1)
}
