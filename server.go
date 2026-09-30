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
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
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

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func serve(listener net.Listener, tlsConfig *tls.Config, handler http.Handler, protocol string) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go handleConn(conn, tlsConfig, handler, protocol)
	}
}

// handleConn 按端口协议分派：http 直接明文服务（含 CONNECT 隧道），
// https 直接进入 TLS 握手按 SNI 现场签发；protocol 留空则按连接首字节
// 自适应识别。serveSingleConn 返回 false（未发生 hijack）时收尾关闭连接。
func handleConn(conn net.Conn, tlsConfig *tls.Config, handler http.Handler, protocol string) {
	var stream net.Conn
	switch protocol {
	case protocolHTTPS:
		if tlsConfig == nil {
			logWarnf("https port requires a certificate, closing remote=%s", conn.RemoteAddr())
			conn.Close()
			return
		}
		stream = tls.Server(conn, tlsConfig)
	case protocolHTTP:
		stream = conn
	default:
		sniffed, ok := sniffConn(conn, tlsConfig)
		if !ok {
			return
		}
		stream = sniffed
	}
	if !serveSingleConn(newHTTPServer(handler), stream) {
		conn.Close()
	}
}

// sniffConn 探测连接首字节是否为 TLS 握手：是则包一层 tls.Server（SNI 现场签发），
// 否则原样返回带预读缓冲的明文连接。探测失败或 TLS 而无证书时关闭连接并返回 false。
func sniffConn(conn net.Conn, tlsConfig *tls.Config) (net.Conn, bool) {
	buffered := &bufferedConn{Conn: conn, reader: bufio.NewReader(conn)}
	buffered.SetReadDeadline(time.Now().Add(protoDetectWait))
	first, err := buffered.reader.Peek(1)
	buffered.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return nil, false
	}
	if first[0] != tlsRecordHandshake {
		return buffered, true
	}
	if tlsConfig == nil {
		logWarnf("received TLS connection but no certificate configured, closing remote=%s", conn.RemoteAddr())
		conn.Close()
		return nil, false
	}
	return tls.Server(buffered, tlsConfig), true
}

// serveSingleConn 在单条连接上跑一次 http.Server.Serve，返回是否发生过 hijack
func serveSingleConn(server *http.Server, conn net.Conn) bool {
	listener := newOneConnListener(conn)
	finish := sync.OnceFunc(listener.finish)
	var hijacked atomic.Bool
	server.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateHijacked {
			hijacked.Store(true)
		}
		if state == http.StateClosed || state == http.StateHijacked {
			finish()
		}
	}
	_ = server.Serve(listener)
	return hijacked.Load()
}

var httpErrorLog = log.New(os.Stderr, "", 0)

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// 去掉标准 log 的日期前缀，底层错误按各自级别落到 stderr，不与控制台日志格式混用
		ErrorLog: httpErrorLog,
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

var serialLimit = new(big.Int).Lsh(big.NewInt(1), 128)

// certCall 承载一次进行中的签发，让同域名并发握手等待同一结果而非重复签发
type certCall struct {
	done chan struct{}
	cert *tls.Certificate
	err  error
}

func newCertCall() *certCall { return &certCall{done: make(chan struct{})} }

func (c *certCall) wait() (*tls.Certificate, error) {
	<-c.done
	return c.cert, c.err
}

func (c *certCall) deliver(cert *tls.Certificate, err error) {
	c.cert, c.err = cert, err
	close(c.done)
}

type certificateAuthority struct {
	cert     *x509.Certificate
	key      crypto.Signer
	mu       sync.Mutex
	cache    *expiringCache[*tls.Certificate]
	inflight map[string]*certCall
}

func newCertificateAuthority(cert *x509.Certificate, key crypto.Signer) *certificateAuthority {
	return &certificateAuthority{
		cert:     cert,
		key:      key,
		cache:    newExpiringCache[*tls.Certificate](maxCachedCerts, serverCertTTL),
		inflight: map[string]*certCall{},
	}
}

// getCertificate 命中缓存即返回；未命中时同域名并发握手合并为一次签发。
// ECDSA 密钥生成与签名耗时较长，须在锁外执行，避免串行化所有域名的握手。
func (ca *certificateAuthority) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	serverName := strings.ToLower(hello.ServerName)
	if serverName == "" {
		return nil, errors.New("missing SNI, cannot sign certificate for domain")
	}
	if cert, ok := ca.cache.get(serverName); ok {
		return cert, nil
	}
	call, leader := ca.begin(serverName)
	if !leader {
		return call.wait()
	}
	cert, err := ca.sign(serverName)
	if err == nil {
		ca.cache.put(serverName, cert)
	}
	ca.end(serverName, call, cert, err)
	return cert, err
}

// begin 登记一次进行中的签发，返回的第二个值表示调用方是否为首个等待者。
// 首个等待者负责签发，其余等待者复用同一结果。
func (ca *certificateAuthority) begin(serverName string) (*certCall, bool) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if call, ok := ca.inflight[serverName]; ok {
		return call, false
	}
	call := newCertCall()
	ca.inflight[serverName] = call
	return call, true
}

// end 注销进行中的签发并广播结果，唤醒所有等待该域名的握手
func (ca *certificateAuthority) end(serverName string, call *certCall, cert *tls.Certificate, err error) {
	ca.mu.Lock()
	delete(ca.inflight, serverName)
	ca.mu.Unlock()
	call.deliver(cert, err)
}

// clearCache 清空已签发证书缓存，热加载后调用以丢弃旧状态
func (ca *certificateAuthority) clearCache() {
	ca.cache.clear()
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

// statusRecorder 记录响应状态码供请求日志使用，其余能力透传给底层 writer；
// 提供 Unwrap，使包装链上的 flusher/hijacker 等接口继续可用。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
