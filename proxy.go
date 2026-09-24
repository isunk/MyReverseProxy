package main

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
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
	io.WriteString(writer, "502 Bad Gateway")
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
	_, matched := p.table.Load().pick(domain, "/")
	if !matched {
		slog.Info("connect", "target", request.Host, "mode", "tunnel")
		if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err == nil {
			p.tunnel(client, request.Host)
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
	serveTLSConn(client, p.tlsConfig, p)
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
