package main

import (
	"crypto/tls"
	"flag"
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
	configPath := flag.String("config", "routing.yaml", "路由配置文件路径")
	addr := flag.String("listen", ":443", "监听地址，按首个字节自动识别 HTTP 与 TLS")
	tlsCert := flag.String("tls-cert", "", "TLS 证书路径，用于 HTTPS 直连的 MITM")
	tlsKey := flag.String("tls-key", "", "TLS 私钥路径")
	logLevel := flag.String("log-level", "info", "日志级别 debug/info/warn/error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fatal("非法日志级别 "+*logLevel, err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	listenAddr = *addr

	transportVerify := http.DefaultTransport.(*http.Transport).Clone()
	transportVerify.Proxy = nil
	transportVerify.DialContext = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	transportVerify.ResponseHeaderTimeout = 30 * time.Second
	transportInsecure := transportVerify.Clone()
	transportInsecure.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	var tlsConfig *tls.Config
	if *tlsCert != "" || *tlsKey != "" {
		if *tlsCert == "" || *tlsKey == "" {
			fatal("--tls-cert 与 --tls-key 必须成对提供", nil)
		}
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			fatal("加载 TLS 证书失败", err)
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"http/1.1"},
			MinVersion:   tls.VersionTLS12,
		}
	}

	instance, err := newProxy(*configPath, transportVerify, transportInsecure, tlsConfig)
	if err != nil {
		fatal("加载路由配置失败", err)
	}
	run(instance)
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
