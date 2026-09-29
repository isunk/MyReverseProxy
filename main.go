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

func main() {
	configPath := flag.String("config", "config.yaml", "routing config file, created automatically when missing")
	port := flag.Int("port", 4000, "listen port, HTTP and TLS detected per connection")
	certPath := flag.String("cert", "ca.crt", "CA certificate file for MITM signing")
	keyPath := flag.String("key", "ca.key", "CA private key file")
	logLevel := flag.String("log", "info", "log level: debug, info, warn or error")
	flag.Parse()

	level, err := parseLogLevel(*logLevel)
	if err != nil {
		fatalf("invalid log level %q: %v", *logLevel, err)
	}
	initLogging(level)

	specified := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { specified[f.Name] = true })

	if !specified["config"] {
		if err := ensureConfig(*configPath); err != nil {
			fatalf("failed to initialize routing config: %v", err)
		}
	}

	certSet, keySet := specified["cert"], specified["key"]
	if certSet != keySet {
		fatalf("--cert and --key must be set together")
	}

	nameservers, err := newNameserverSet(5*time.Second, defaultNameservers)
	if err != nil {
		fatalf("failed to initialize dns nameservers: %v", err)
	}

	transport := newTransport(nameservers.DialContext)

	tlsConfig, authority, err := resolveTLSConfig(*certPath, *keyPath, certSet && keySet)
	if err != nil {
		fatalf("TLS certificate configuration error: %v", err)
	}

	instance, err := newProxy(*configPath, transport, tlsConfig, authority, nameservers)
	if err != nil {
		fatalf("failed to load routing config: %v", err)
	}
	logInfof("dns nameservers=%s", strings.Join(nameservers.serverAddresses(), ","))
	run(instance, fmt.Sprintf(":%d", *port))
}

func newTransport(dialContext func(context.Context, string, string) (net.Conn, error)) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // 禁用环境代理，避免代理流量经上游代理回环到自身
	transport.DialContext = dialContext
	transport.ResponseHeaderTimeout = 30 * time.Second
	// 默认 MaxIdleConnsPerHost=2，代理到同一上游的并发请求会频繁重建连接（TCP+TLS 握手）
	transport.MaxIdleConns = 256
	transport.MaxIdleConnsPerHost = 64
	transport.ReadBufferSize = 32 << 10
	transport.WriteBufferSize = 32 << 10
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // mrp 位于设备与上游之间，上游证书校验交由设备端完成
	return transport
}

func resolveTLSConfig(certPath, keyPath string, explicitPair bool) (*tls.Config, *certificateAuthority, error) {
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
	return loadTLSConfig(certPath, keyPath)
}

func loadTLSConfig(certPath, keyPath string) (*tls.Config, *certificateAuthority, error) {
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

func run(p *proxy, listenAddr string) {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fatalf("listen failed: %v", err)
	}
	logInfof("listening addr=%s", listenAddr)
	go serveListener(listener, p)
	go p.watchFile(time.Second, nil)
	serveSignals(p, listener)
}

func serveListener(listener net.Listener, p *proxy) {
	if err := serve(listener, p.tlsConfig, p); err != nil && !errors.Is(err, net.ErrClosed) {
		fatalf("server exited: %v", err)
	}
}

func serveSignals(p *proxy, listener net.Listener) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for sig := range sigCh {
		if sig != syscall.SIGHUP {
			listener.Close()
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
