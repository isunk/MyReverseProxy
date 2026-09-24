package main

import (
	"crypto/tls"
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

var (
	listenAddr string
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

	listenAddr = fmt.Sprintf(":%d", *port)

	transportVerify := http.DefaultTransport.(*http.Transport).Clone()
	transportVerify.Proxy = nil
	transportVerify.DialContext = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	transportVerify.ResponseHeaderTimeout = 30 * time.Second
	transportInsecure := transportVerify.Clone()
	transportInsecure.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	tlsConfig, err := resolveTLSConfig(*tlsCert, *tlsKey, specified["tls-cert"], specified["tls-key"])
	if err != nil {
		fatal("TLS 证书配置错误", err)
	}

	instance, err := newProxy(*configPath, transportVerify, transportInsecure, tlsConfig)
	if err != nil {
		fatal("加载路由配置失败", err)
	}
	run(instance)
}

func resolveTLSConfig(tlsCert, tlsKey string, certSet, keySet bool) (*tls.Config, error) {
	if certSet || keySet {
		if !certSet || !keySet {
			return nil, fmt.Errorf("--tls-cert 与 --tls-key 必须成对提供")
		}
		return loadTLSConfig(tlsCert, tlsKey)
	}
	certExists := fileExists(tlsCert)
	keyExists := fileExists(tlsKey)
	switch {
	case certExists && keyExists:
		return loadTLSConfig(tlsCert, tlsKey)
	case certExists != keyExists:
		return nil, fmt.Errorf("证书与私钥必须成对存在：%s / %s", tlsCert, tlsKey)
	default:
		slog.Warn("未找到默认证书，仅支持 HTTP 与 CONNECT 隧道", "cert", tlsCert, "key", tlsKey)
		return nil, nil
	}
}

func loadTLSConfig(tlsCert, tlsKey string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"http/1.1"},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func ensureConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(path, []byte(defaultConfig), 0o644); err != nil {
		return err
	}
	slog.Info("已创建默认路由配置文件", "path", path)
	return nil
}

func run(p *proxy) {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fatal("监听失败", err)
	}
	slog.Info("监听", "addr", listenAddr)
	go func() {
		if err := serve(listener, p.tlsConfig, p); err != nil {
			fatal("服务退出", err)
		}
	}()
	go p.watchFile(time.Second, nil)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for sig := range sigCh {
		if sig == syscall.SIGHUP {
			if err := p.reload(); err != nil {
				slog.Error("热加载失败，沿用当前配置", "error", err)
			} else {
				slog.Info("路由配置已热加载")
			}
			continue
		}
		return
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
