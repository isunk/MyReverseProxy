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
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type oneConnListener struct {
	conn  net.Conn
	taken atomic.Bool
	done  chan struct{}
}

// TLS 记录层首字节 0x16 表示 handshake，即 ClientHello 报文
const tlsRecordHandshake = 0x16

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
	if first[0] == tlsRecordHandshake {
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
	serveSingleConn(&http.Server{Handler: handler}, tls.Server(raw, tlsConfig))
}

func domainOf(request *http.Request) string {
	if request.TLS != nil && request.TLS.ServerName != "" {
		return request.TLS.ServerName
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

const (
	serverCertTTL  = 24 * time.Hour
	maxCachedCerts = 512
)

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
		return nil, errors.New("缺少 SNI，无法按域名签发证书")
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

func (l *logWriter) WriteHeader(code int) {
	l.status = code
	l.ResponseWriter.WriteHeader(code)
}

func (l *logWriter) Unwrap() http.ResponseWriter {
	return l.ResponseWriter
}
