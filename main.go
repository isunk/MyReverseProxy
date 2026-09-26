package main

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
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

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fatal("invalid log level "+*logLevel, err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	specified := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { specified[f.Name] = true })

	if !specified["config"] {
		if err := ensureConfig(*configPath); err != nil {
			fatal("failed to initialize routing config", err)
		}
	}

	certSet, keySet := specified["cert"], specified["key"]
	if certSet != keySet {
		fatal("--cert and --key must be set together", nil)
	}

	transport := newTransport(5 * time.Second)

	tlsConfig, err := resolveTLSConfig(*certPath, *keyPath, certSet && keySet)
	if err != nil {
		fatal("TLS certificate configuration error", err)
	}

	instance, err := newProxy(*configPath, transport, tlsConfig)
	if err != nil {
		fatal("failed to load routing config", err)
	}
	run(instance, fmt.Sprintf(":%d", *port))
}

func newTransport(dialTimeout time.Duration) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // 禁用环境代理，避免代理流量经上游代理回环到自身
	transport.DialContext = (&net.Dialer{Timeout: dialTimeout}).DialContext
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // mrp 位于设备与上游之间，上游证书校验交由设备端完成
	return transport
}

func resolveTLSConfig(certPath, keyPath string, explicit bool) (*tls.Config, error) {
	if !explicit {
		certExists := fileExists(certPath)
		keyExists := fileExists(keyPath)
		if !certExists && !keyExists {
			slog.Warn("no default certificate found, serving HTTP and CONNECT tunnel only", "cert", certPath, "key", keyPath)
			return nil, nil
		}
		if certExists != keyExists {
			return nil, fmt.Errorf("certificate and key must exist as a pair: %s / %s", certPath, keyPath)
		}
	}
	return loadTLSConfig(certPath, keyPath)
}

func loadTLSConfig(certPath, keyPath string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("unsupported private key type")
	}
	authority := newCertificateAuthority(caCert, signer)
	return &tls.Config{
		GetCertificate: authority.getCertificate,
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}, nil
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
	slog.Info("created default config file", "path", path)
	return nil
}

func run(p *proxy, listenAddr string) {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fatal("listen failed", err)
	}
	slog.Info("listening", "addr", listenAddr)
	go serveListener(listener, p)
	go p.watchFile(time.Second, nil)
	serveSignals(p)
}

func serveListener(listener net.Listener, p *proxy) {
	if err := serve(listener, p.tlsConfig, p); err != nil {
		fatal("server exited", err)
	}
}

func serveSignals(p *proxy) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for sig := range sigCh {
		if sig != syscall.SIGHUP {
			return
		}
		if err := p.reload(); err != nil {
			slog.Error("hot reload failed, keeping current config", "error", err)
			continue
		}
		slog.Info("routing config reloaded")
	}
}

func fatal(message string, err error) {
	if err != nil {
		slog.Error(message, "error", err)
	} else {
		slog.Error(message)
	}
	os.Exit(1)
}
