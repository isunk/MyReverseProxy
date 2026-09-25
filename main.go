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
	configPath := flag.String("config", "routing.yaml", "路由配置文件路径，未指定时默认当前目录 routing.yaml，缺失则自动创建")
	port := flag.Int("port", 443, "监听端口，按首个字节自动识别 HTTP 与 TLS")
	tlsCert := flag.String("tls-cert", "ca.crt", "TLS 证书路径，用于 HTTPS 直连的 MITM")
	tlsKey := flag.String("tls-key", "ca.key", "TLS 私钥路径")
	logLevel := flag.String("log-level", "info", "日志级别 debug/info/warn/error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fatal("非法日志级别 "+*logLevel, err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	specified := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { specified[f.Name] = true })

	if !specified["config"] {
		if err := ensureConfig(*configPath); err != nil {
			fatal("初始化路由配置失败", err)
		}
	}

	certSet, keySet := specified["tls-cert"], specified["tls-key"]
	if certSet != keySet {
		fatal("--tls-cert 与 --tls-key 必须成对提供", nil)
	}

	transportVerify, transportInsecure := newTransports(5 * time.Second)

	tlsConfig, err := resolveTLSConfig(*tlsCert, *tlsKey, certSet && keySet)
	if err != nil {
		fatal("TLS 证书配置错误", err)
	}

	instance, err := newProxy(*configPath, transportVerify, transportInsecure, tlsConfig)
	if err != nil {
		fatal("加载路由配置失败", err)
	}
	run(instance, fmt.Sprintf(":%d", *port))
}

func newTransports(dialTimeout time.Duration) (verify, insecure *http.Transport) {
	verify = http.DefaultTransport.(*http.Transport).Clone()
	verify.Proxy = nil // 禁用环境代理，避免代理流量经上游代理回环到自身
	verify.DialContext = (&net.Dialer{Timeout: dialTimeout}).DialContext
	verify.ResponseHeaderTimeout = 30 * time.Second
	insecure = verify.Clone()
	insecure.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return verify, insecure
}

func resolveTLSConfig(tlsCert, tlsKey string, explicit bool) (*tls.Config, error) {
	if !explicit {
		certExists := fileExists(tlsCert)
		keyExists := fileExists(tlsKey)
		if !certExists && !keyExists {
			slog.Warn("未找到默认证书，仅支持 HTTP 与 CONNECT 隧道", "cert", tlsCert, "key", tlsKey)
			return nil, nil
		}
		if certExists != keyExists {
			return nil, fmt.Errorf("证书与私钥必须成对存在：%s / %s", tlsCert, tlsKey)
		}
	}
	return loadTLSConfig(tlsCert, tlsKey)
}

func loadTLSConfig(tlsCert, tlsKey string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("私钥类型不支持")
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
	slog.Info("已创建默认路由配置文件", "path", path)
	return nil
}

func run(p *proxy, listenAddr string) {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fatal("监听失败", err)
	}
	slog.Info("监听", "addr", listenAddr)
	go serveListener(listener, p)
	go p.watchFile(time.Second, nil)
	serveSignals(p)
}

func serveListener(listener net.Listener, p *proxy) {
	if err := serve(listener, p.tlsConfig, p); err != nil {
		fatal("服务退出", err)
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
			slog.Error("热加载失败，沿用当前配置", "error", err)
			continue
		}
		slog.Info("路由配置已热加载")
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
