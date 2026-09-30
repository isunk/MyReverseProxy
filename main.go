package main

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// startupOptions 命令行解析结果，构建代理实例所需的全部启动参数。
type startupOptions struct {
	configPath      string
	certPath        string
	keyPath         string
	logLevel        logLevel
	dnsTimeout      time.Duration
	dnsTTL          time.Duration
	configSpecified bool
	certSpecified   bool
	keySpecified    bool
}

// parseStartupOptions 解析并校验命令行参数，非法取值在此集中报错。
func parseStartupOptions() (startupOptions, error) {
	var opts startupOptions
	levelName := ""
	flag.StringVar(&opts.configPath, "config", "config.yaml", "routing config file, created automatically when missing")
	flag.StringVar(&opts.certPath, "cert", "ca.crt", "CA certificate file for MITM signing")
	flag.StringVar(&opts.keyPath, "key", "ca.key", "CA private key file")
	flag.StringVar(&levelName, "log", "info", "log level: debug, info, warn or error")
	flag.DurationVar(&opts.dnsTimeout, "dns-timeout", defaultAttemptTimeout, "per-nameserver attempt timeout before failing over to the next")
	flag.DurationVar(&opts.dnsTTL, "dns-ttl", defaultDNSTTL, "ttl of cached upstream resolutions, 0 disables the cache")
	flag.Parse()
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "config":
			opts.configSpecified = true
		case "cert":
			opts.certSpecified = true
		case "key":
			opts.keySpecified = true
		}
	})
	level, err := parseLogLevel(levelName)
	if err != nil {
		return opts, err
	}
	opts.logLevel = level
	if opts.dnsTimeout <= 0 {
		return opts, fmt.Errorf("invalid --dns-timeout %v: must be positive", opts.dnsTimeout)
	}
	if opts.dnsTTL < 0 {
		return opts, fmt.Errorf("invalid --dns-ttl %v: must be zero or positive", opts.dnsTTL)
	}
	return opts, nil
}

// buildProxy 组装代理实例：初始化配置与 DNS 服务器、构建上游连接池与路由表。
func buildProxy(opts startupOptions) (*proxy, error) {
	if !opts.configSpecified {
		if err := ensureConfig(opts.configPath); err != nil {
			return nil, fmt.Errorf("initialize routing config: %w", err)
		}
	}
	if opts.certSpecified != opts.keySpecified {
		return nil, errors.New("--cert and --key must be set together")
	}
	nameservers, err := newNameserverSet(opts.dnsTimeout, opts.dnsTTL, defaultNameservers)
	if err != nil {
		return nil, fmt.Errorf("initialize dns nameservers: %w", err)
	}
	logInfof("dns nameservers=%s dns-ttl=%s", strings.Join(nameservers.serverAddresses(), ","), opts.dnsTTL)
	transport := newTransport(nameservers.DialContext)
	tlsConfig, authority, err := loadTLSConfig(opts.certPath, opts.keyPath, opts.certSpecified)
	if err != nil {
		return nil, fmt.Errorf("tls certificate configuration: %w", err)
	}
	return newProxy(opts.configPath, transport, tlsConfig, authority, nameservers)
}

func main() {
	opts, err := parseStartupOptions()
	if err != nil {
		fatalf("invalid flags: %v", err)
	}
	initLogging(opts.logLevel)
	proxy, err := buildProxy(opts)
	if err != nil {
		fatalf("startup failed: %v", err)
	}
	run(proxy)
}

const (
	maxCachedTLSSessions   = 64
	responseHeaderTimeout  = 30 * time.Second
	maxIdleConnections     = 256
	maxIdleConnectionsHost = 64
	transportBufferSize    = 32 << 10
)

// newTransport 构建上游连接池：关闭环境代理防回环、放宽同主机连接复用，
// 并开启 TLS 会话复用，避免每条新上游连接重跑完整握手。
func newTransport(dialContext func(context.Context, string, string) (net.Conn, error)) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // 禁用环境代理，避免代理流量经上游代理回环到自身
	transport.DialContext = dialContext
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	// 默认 MaxIdleConnsPerHost=2，代理到同一上游的并发请求会频繁重建连接（TCP+TLS 握手）
	transport.MaxIdleConns = maxIdleConnections
	transport.MaxIdleConnsPerHost = maxIdleConnectionsHost
	transport.ReadBufferSize = transportBufferSize
	transport.WriteBufferSize = transportBufferSize
	// ClientSessionCache 为 nil 时 Go 禁用会话复用，每次新上游连接都要走完整 TLS 握手
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true, // mrp 位于设备与上游之间，上游证书校验交由设备端完成
		ClientSessionCache: tls.NewLRUClientSessionCache(maxCachedTLSSessions),
	}
	return transport
}

// loadTLSConfig 加载 CA 证书与私钥并构建现场签发能力。explicitPair 为 true 时
// 跳过文件存在性检查（调用方已确认 --cert/--key 同时指定）；为 false 时，
// 两个文件都不存在则返回 nil 配置进入纯 HTTP/隧道模式，只存在一个则报错。
func loadTLSConfig(certPath, keyPath string, explicitPair bool) (*tls.Config, *certificateAuthority, error) {
	if !explicitPair {
		certExists := fileExists(certPath)
		keyExists := fileExists(keyPath)
		if !certExists && !keyExists {
			logWarnf("no default certificate found, serving HTTP and CONNECT tunnel only cert=%s key=%s", certPath, keyPath)
			return nil, nil, nil
		}
		if certExists != keyExists {
			return nil, nil, fmt.Errorf("certificate and key must exist as a pair: %s / %s", certPath, keyPath)
		}
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load key pair: %w", err)
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
	authority := newCertificateAuthority(caCert, signer)
	return &tls.Config{
		GetCertificate: authority.getCertificate,
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}, authority, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func ensureConfig(path string) error {
	_, err := os.Stat(path)
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(path, []byte(defaultConfig), 0o644); err != nil {
		return err
	}
	logInfof("created default config file path=%s", path)
	return nil
}

// run 监听由 reload 内的 listenerSet 管理：newProxy 启动期已建立首批监听，
// 此后仅轮询配置热加载并等待信号。退出信号回收监听集后返回。
func run(p *proxy) {
	go p.watchFile(time.Second, nil)
	serveSignals(p)
}

func serveSignals(p *proxy) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for sig := range sigCh {
		if sig != syscall.SIGHUP {
			p.listeners.closeAll()
			return
		}
		if err := p.reload(); err != nil {
			logErrorf("hot reload failed, keeping current config: %v", err)
			continue
		}
		logInfof("routing config reloaded")
	}
}

func fatalf(format string, args ...any) {
	logErrorf(format, args...)
	os.Exit(1)
}
