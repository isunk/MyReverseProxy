package ca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"mrp/internal/cache"
	"mrp/internal/log"
)

const (
	certTTL        = 24 * time.Hour
	maxCachedCerts = 512
)

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

// Authority 按客户端 SNI 用持有的 CA 现场签发服务端证书，签发结果按域名缓存。
type Authority struct {
	cert     *x509.Certificate
	key      crypto.Signer
	mu       sync.Mutex
	cache    *cache.Cache[*tls.Certificate]
	inflight map[string]*certCall
}

func New(cert *x509.Certificate, key crypto.Signer) *Authority {
	return &Authority{
		cert:     cert,
		key:      key,
		cache:    cache.New[*tls.Certificate](maxCachedCerts, certTTL),
		inflight: map[string]*certCall{},
	}
}

// GetCertificate 供标准 TLS 握手按 SNI 取证书；SNI 缺失时报错，
// 由每连接 TLS 配置在 IP 直连客户端不发 SNI 时以 CONNECT 目标回退。
func (a *Authority) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return a.CertificateFor(hello.ServerName)
}

// CertificateFor 按显式名字签发或取缓存证书：名字可来自 SNI，也可来自 CONNECT 目标
// （客户端对 IP 直连不发 SNI）。命中缓存即返回；未命中时同名字并发握手合并为一次签发。
// ECDSA 密钥生成与签名耗时较长，须在锁外执行，避免串行化所有域名的握手。
func (a *Authority) CertificateFor(serverName string) (*tls.Certificate, error) {
	serverName = strings.ToLower(serverName)
	if serverName == "" {
		return nil, errors.New("missing SNI, cannot sign certificate for domain")
	}
	if cert, ok := a.cache.Get(serverName); ok {
		return cert, nil
	}
	call, leader := a.begin(serverName)
	if !leader {
		return call.wait()
	}
	var cert *tls.Certificate
	var err error
	// end 用 defer 兜底：sign 意外 panic 时 net/http 连接层会 recover，进程存活，
	// 若不注销 inflight，该域名的所有后续握手将永久阻塞在 wait 上
	defer func() { a.end(serverName, call, cert, err) }()
	// 双检：Get 未命中到取得 leader 之间，可能已有上一轮签发完成并写入缓存
	if cached, ok := a.cache.Get(serverName); ok {
		cert = cached
		return cached, nil
	}
	cert, err = a.sign(serverName)
	if err == nil {
		a.cache.Put(serverName, cert)
	}
	return cert, err
}

// begin 登记一次进行中的签发，返回的第二个值表示调用方是否为首个等待者。
// 首个等待者负责签发，其余等待者复用同一结果。
func (a *Authority) begin(serverName string) (*certCall, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if call, ok := a.inflight[serverName]; ok {
		return call, false
	}
	call := newCertCall()
	a.inflight[serverName] = call
	return call, true
}

// end 注销进行中的签发并广播结果，唤醒所有等待该域名的握手
func (a *Authority) end(serverName string, call *certCall, cert *tls.Certificate, err error) {
	a.mu.Lock()
	delete(a.inflight, serverName)
	a.mu.Unlock()
	call.deliver(cert, err)
}

// ClearCache 清空已签发证书缓存，热加载后调用以丢弃旧状态
func (a *Authority) ClearCache() {
	a.cache.Clear()
}

// sign 现场签发单个域名的证书：每次生成独立密钥不复用，NotBefore 回拨一小时容忍时钟偏移，
// 证书链带上 CA 根证书使客户端无需额外安装即可信任。
func (a *Authority) sign(serverName string) (*tls.Certificate, error) {
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
		NotAfter:     time.Now().Add(certTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	// 名字为 IP 字面量时按 IP SAN 签发：客户端校验 https://<ip> 走 IPAddresses 而非 DNSNames
	if ip := net.ParseIP(serverName); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{serverName}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &priv.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, a.cert.Raw},
		PrivateKey:  priv,
	}, nil
}

// Load 加载 CA 证书与私钥并构建现场签发能力。explicitPair 为 true 时
// 跳过文件存在性检查（调用方已确认 --cert/--key 同时指定）；为 false 时，
// 两个文件都不存在则返回 nil 配置进入纯 HTTP/隧道模式，只存在一个则报错。
func Load(certPath, keyPath string, explicitPair bool) (*tls.Config, *Authority, error) {
	if !explicitPair {
		certExists := fileExists(certPath)
		keyExists := fileExists(keyPath)
		if !certExists && !keyExists {
			log.Warn("no default certificate found, serving HTTP and CONNECT tunnel only cert=%s key=%s", certPath, keyPath)
			return nil, nil, nil
		}
		if certExists != keyExists {
			return nil, nil, fmt.Errorf("certificate and key must exist as a pair: %s / %s", certPath, keyPath)
		}
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load key pair: %v", err)
	}
	caCert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, fmt.Errorf("parse certificate: %w", err)
	}
	if !caCert.IsCA {
		return nil, nil, errors.New("certificate is not a CA, cannot sign certificates for SNI")
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("unsupported private key type")
	}
	authority := New(caCert, signer)
	return &tls.Config{
		GetCertificate: authority.GetCertificate,
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}, authority, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
