package main

import (
	"bufio"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TLS 记录层首字节 0x16 表示 handshake，即 ClientHello 报文
const tlsRecordHandshake = 0x16

const (
	serverCertTTL     = 24 * time.Hour
	maxCachedCerts    = 512
	protoDetectWait   = 10 * time.Second
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 60 * time.Second
)

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
	close(l.done)
}

func (l *oneConnListener) Close() error { return nil }

func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func newBufferedConn(conn net.Conn) *bufferedConn {
	return &bufferedConn{Conn: conn, reader: bufio.NewReader(conn)}
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

// handleConn 按连接首字节识别协议：TLS 握手包包一层 tls.Server 后与纯 HTTP 走同一服务管道，
// hijack 发生时连接所有权移交给内层 handler，返回 false 则由这里收尾关闭
func handleConn(conn net.Conn, tlsConfig *tls.Config, handler http.Handler) {
	buffered := newBufferedConn(conn)
	isTLS, err := sniffTLS(buffered)
	if err != nil {
		conn.Close()
		return
	}
	if isTLS && tlsConfig == nil {
		logWarnf("received TLS connection but no certificate configured, closing remote=%s", conn.RemoteAddr())
		conn.Close()
		return
	}
	stream := net.Conn(buffered)
	if isTLS {
		stream = tls.Server(buffered, tlsConfig)
	}
	if !serveSingleConn(newHTTPServer(handler), stream) {
		conn.Close()
	}
}

func sniffTLS(conn *bufferedConn) (bool, error) {
	conn.SetReadDeadline(time.Now().Add(protoDetectWait))
	first, err := conn.reader.Peek(1)
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		return false, err
	}
	return first[0] == tlsRecordHandshake, nil
}

// serveSingleConn 在单条连接上跑一次 http.Server.Serve，返回是否发生过 hijack
func serveSingleConn(server *http.Server, conn net.Conn) bool {
	listener := newOneConnListener(conn)
	var once sync.Once
	var hijacked atomic.Bool
	server.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateHijacked {
			hijacked.Store(true)
		}
		if state == http.StateClosed || state == http.StateHijacked {
			once.Do(listener.finish)
		}
	}
	_ = server.Serve(listener)
	return hijacked.Load()
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
}

func domainOf(request *http.Request) string {
	if request.TLS != nil && request.TLS.ServerName != "" {
		return strings.ToLower(request.TLS.ServerName)
	}
	return hostOnly(request.Host)
}

func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(host)
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

var serialLimit = new(big.Int).Lsh(big.NewInt(1), 128)

type cacheEntry struct {
	cert      *tls.Certificate
	expiresAt time.Time
}

type certificateAuthority struct {
	cert  *x509.Certificate
	key   crypto.Signer
	mu    sync.Mutex
	cache map[string]cacheEntry
}

func newCertificateAuthority(cert *x509.Certificate, key crypto.Signer) *certificateAuthority {
	return &certificateAuthority{cert: cert, key: key, cache: map[string]cacheEntry{}}
}

func (ca *certificateAuthority) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello.ServerName == "" {
		return nil, errors.New("missing SNI, cannot sign certificate for domain")
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if entry, ok := ca.cache[hello.ServerName]; ok && time.Now().Before(entry.expiresAt) {
		return entry.cert, nil
	}
	cert, err := ca.sign(hello.ServerName)
	if err != nil {
		return nil, err
	}
	if len(ca.cache) >= maxCachedCerts {
		ca.cache = map[string]cacheEntry{}
	}
	ca.cache[hello.ServerName] = cacheEntry{cert: cert, expiresAt: time.Now().Add(serverCertTTL)}
	return cert, nil
}

func (ca *certificateAuthority) sign(serverName string) (*tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: serverName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(serverCertTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{serverName},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &priv.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, ca.cert.Raw},
		PrivateKey:  priv,
	}, nil
}

type logWriter struct {
	http.ResponseWriter
	status int
}

func (w *logWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *logWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
