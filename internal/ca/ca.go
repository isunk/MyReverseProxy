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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/isunk/MyReverseProxy/internal/cache"
	"github.com/isunk/MyReverseProxy/internal/logging"
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

// GetCertificate 命中缓存即返回；未命中时同域名并发握手合并为一次签发。
// ECDSA 密钥生成与签名耗时较长，须在锁外执行，避免串行化所有域名的握手。
func (a *Authority) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	serverName := strings.ToLower(hello.ServerName)
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
	cert, err := a.sign(serverName)
	if err == nil {
		a.cache.Put(serverName, cert)
	}
	a.end(serverName, call, cert, err)
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
		DNSNames:     []string{serverName},
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
			logging.Warnf("no default certificate found, serving HTTP and CONNECT tunnel only cert=%s key=%s", certPath, keyPath)
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
