package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
)

type context_key int

const sni_key context_key = 1

type one_conn_listener struct {
	conn net.Conn
}

func new_one_conn_listener(conn net.Conn) *one_conn_listener {
	return &one_conn_listener{conn: conn}
}

func (l *one_conn_listener) Accept() (net.Conn, error) { return l.conn, nil }

func (l *one_conn_listener) Close() error { return l.conn.Close() }

func (l *one_conn_listener) Addr() net.Addr { return l.conn.LocalAddr() }

func serve_https(listener net.Listener, tls_config *tls.Config, handler http.Handler) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go serve_tls_with_sni(conn, tls_config, handler)
	}
}

func serve_tls_with_sni(raw net.Conn, tls_config *tls.Config, handler http.Handler) {
	defer raw.Close()
	var sni string
	cfg := tls_config.Clone()
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		sni = hello.ServerName
		return nil, nil
	}
	tls_conn := tls.Server(raw, cfg)
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if sni != "" {
			request = request.WithContext(with_sni(request, sni))
		}
		handler.ServeHTTP(writer, request)
	})}
	_ = server.Serve(new_one_conn_listener(tls_conn))
}

func with_sni(request *http.Request, sni string) context.Context {
	return context.WithValue(request.Context(), sni_key, sni)
}

func domain_of(request *http.Request) string {
	if value, ok := request.Context().Value(sni_key).(string); ok && value != "" {
		return value
	}
	return host_only(request.Host)
}

func host_only(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func join_path(base, rest string) string {
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

type log_writer struct {
	http.ResponseWriter
	status int
}

func (l *log_writer) WriteHeader(code int) {
	l.status = code
	l.ResponseWriter.WriteHeader(code)
}

func (l *log_writer) Unwrap() http.ResponseWriter {
	return l.ResponseWriter
}
