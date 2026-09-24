package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
)

type ctxKey int

const sniKey ctxKey = 1

type oneConnListener struct {
	conn net.Conn
}

func newOneConnListener(conn net.Conn) *oneConnListener {
	return &oneConnListener{conn: conn}
}

func (l *oneConnListener) Accept() (net.Conn, error) { return l.conn, nil }

func (l *oneConnListener) Close() error { return l.conn.Close() }

func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

func serveHTTPS(listener net.Listener, tlsConfig *tls.Config, handler http.Handler) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go serveTLSConn(conn, tlsConfig, handler)
	}
}

func serveTLSConn(raw net.Conn, tlsConfig *tls.Config, handler http.Handler) {
	defer raw.Close()
	var sni string
	cfg := tlsConfig.Clone()
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		sni = hello.ServerName
		return nil, nil
	}
	tlsConn := tls.Server(raw, cfg)
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if sni != "" {
			request = request.WithContext(withSNI(request, sni))
		}
		handler.ServeHTTP(writer, request)
	})}
	_ = server.Serve(newOneConnListener(tlsConn))
}

func withSNI(request *http.Request, sni string) context.Context {
	return context.WithValue(request.Context(), sniKey, sni)
}

func domainOf(request *http.Request) string {
	if value, ok := request.Context().Value(sniKey).(string); ok && value != "" {
		return value
	}
	return hostOnly(request.Host)
}

func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
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
