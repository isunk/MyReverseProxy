package main

import (
	"bytes"
	"crypto/tls"
	"io"
	"log/slog"
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
)

type proxy struct {
	configPath        string
	table             atomic.Pointer[routeTable]
	transportVerify   *http.Transport
	transportInsecure *http.Transport
	tlsConfig         *tls.Config
	passthrough       *httputil.ReverseProxy
}

func newProxy(configPath string, transportVerify, transportInsecure *http.Transport, tlsConfig *tls.Config) (*proxy, error) {
	p := &proxy{
		configPath:        configPath,
		transportVerify:   transportVerify,
		transportInsecure: transportInsecure,
		tlsConfig:         tlsConfig,
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
		Transport:    transportVerify,
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
			transport := p.transportVerify
			if entry.insecure {
				transport = p.transportInsecure
			}
			entry.proxy = p.newRouteProxy(entry, transport)
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
			previous = p.syncConfig(previous)
		}
	}
}

func (p *proxy) syncConfig(previous []byte) []byte {
	data, err := os.ReadFile(p.configPath)
	if err != nil {
		slog.Warn("读取路由配置失败", "error", err)
		return previous
	}
	if previous != nil && bytes.Equal(data, previous) {
		return previous
	}
	if err := p.reload(); err != nil {
		slog.Error("配置变更热加载失败，沿用旧配置", "error", err)
	} else {
		slog.Info("检测到配置变更，已热加载")
	}
	return data
}

func (p *proxy) newRouteProxy(entry *route, transport http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(entry.target)
			request.Out.URL.Path = joinPath(entry.target.Path, strings.TrimPrefix(request.In.URL.Path, entry.prefix))
			request.Out.URL.RawPath = ""
			if entry.host != "" {
				request.Out.Host = entry.host
			}
		},
		Transport:    transport,
		ErrorHandler: p.errorHandler,
	}
}

func (p *proxy) errorHandler(writer http.ResponseWriter, request *http.Request, err error) {
	slog.Error("上游请求失败", "host", request.Host, "path", request.URL.Path, "error", err)
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
		slog.Info("request", "domain", domain, "path", request.URL.Path, "status", recorder.status, "elapsed", time.Since(start))
	}()
	if matched {
		slog.Debug("路由命中", "domain", domain, "prefix", entry.prefix, "upstream", entry.target.String())
		entry.proxy.ServeHTTP(recorder, request)
		return
	}
	slog.Debug("路由未命中，透传原始目标", "domain", domain)
	p.passthrough.ServeHTTP(recorder, request)
}

func (p *proxy) handleConnect(writer http.ResponseWriter, request *http.Request) {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		http.Error(writer, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	defer client.Close()

	domain := hostOnly(request.Host)
	if !p.table.Load().has(domain) {
		slog.Info("connect", "target", request.Host, "mode", "tunnel")
		if _, err := client.Write([]byte(connectEstablished)); err != nil {
			return
		}
		p.tunnel(client, request.Host)
		return
	}
	if p.tlsConfig == nil {
		_, _ = client.Write([]byte(connectBadGateway))
		return
	}
	slog.Info("connect", "domain", domain, "mode", "mitm")
	if _, err := client.Write([]byte(connectEstablished)); err != nil {
		return
	}
	serveTLSConn(client, p.tlsConfig, p)
}

func (p *proxy) tunnel(client net.Conn, target string) {
	upstream, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		slog.Error("隧道目标连接失败", "target", target, "error", err)
		_, _ = client.Write([]byte(connectBadGateway))
		return
	}
	defer upstream.Close()
	go func() {
		io.Copy(upstream, client)
		upstream.Close()
	}()
	io.Copy(client, upstream)
}
