package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

type ctxKey int

const sniKey ctxKey = 1

type oneConnListener struct {
	conn  net.Conn
	taken atomic.Bool
	done  chan struct{}
}

func newOneConnListener(conn net.Conn) *oneConnListener {
	return &oneConnListener{conn: conn, done: make(chan struct{})}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.taken.CompareAndSwap(false, true) {
		return l.conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) finish() {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
}

func (l *oneConnListener) Close() error { return nil }

func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func serve(listener net.Listener, tlsConfig *tls.Config, handler http.Handler) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go handleConn(conn, tlsConfig, handler)
	}
}

func handleConn(conn net.Conn, tlsConfig *tls.Config, handler http.Handler) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	buffered := &bufferedConn{Conn: conn, reader: reader}
	first, err := reader.Peek(1)
	if err != nil {
		return
	}
	if first[0] == 0x16 {
		if tlsConfig == nil {
			slog.Warn("收到 TLS 直连请求但未配置证书，已断开", "remote", conn.RemoteAddr())
			return
		}
		serveTLSConn(buffered, tlsConfig, handler)
		return
	}
	serveSingleConn(&http.Server{Handler: handler}, buffered)
}

func serveSingleConn(server *http.Server, conn net.Conn) {
	listener := newOneConnListener(conn)
	var once sync.Once
	server.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			once.Do(listener.finish)
		}
	}
	_ = server.Serve(listener)
}

func serveTLSConn(raw net.Conn, tlsConfig *tls.Config, handler http.Handler) {
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
	serveSingleConn(server, tlsConn)
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
