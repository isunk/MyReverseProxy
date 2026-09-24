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
	httpListen  string
	httpsListen string
)

func main() {
	configPath := flag.String("config", "routing.yaml", "路由配置文件路径")
	httpAddr := flag.String("http", ":80", "HTTP 监听地址，传空禁用")
	httpsAddr := flag.String("https", ":443", "HTTPS 监听地址，传空禁用")
	tlsCert := flag.String("tls-cert", "", "TLS 证书路径，启用 HTTPS 时必填")
	tlsKey := flag.String("tls-key", "", "TLS 私钥路径，启用 HTTPS 时必填")
	logLevel := flag.String("log-level", "info", "日志级别 debug/info/warn/error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fatal("非法日志级别 "+*logLevel, err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	httpListen = *httpAddr
	httpsListen = *httpsAddr

	transportVerify := http.DefaultTransport.(*http.Transport).Clone()
	transportVerify.Proxy = nil
	transportVerify.DialContext = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	transportVerify.ResponseHeaderTimeout = 30 * time.Second
	transportInsecure := transportVerify.Clone()
	transportInsecure.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	var tlsConfig *tls.Config
	if httpsListen != "" {
		if *tlsCert == "" || *tlsKey == "" {
			fatal("启用 HTTPS 监听时必须提供 --tls-cert 与 --tls-key", nil)
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
	if httpListen != "" {
		go func() {
			slog.Info("HTTP 监听", "addr", httpListen)
			if err := http.ListenAndServe(httpListen, p); err != nil {
				fatal("HTTP 监听失败", err)
			}
		}()
	}
	if httpsListen != "" {
		if p.tlsConfig == nil {
			fatal("启用 HTTPS 监听时必须提供 --tls-cert 与 --tls-key", nil)
		}
		listener, err := net.Listen("tcp", httpsListen)
		if err != nil {
			fatal("HTTPS 监听失败", err)
		}
		slog.Info("HTTPS 监听", "addr", httpsListen)
		go func() {
			if err := serveHTTPS(listener, p.tlsConfig, p); err != nil {
				fatal("HTTPS 服务退出", err)
			}
		}()
	}
	if httpListen == "" && httpsListen == "" {
		fatal("HTTP 与 HTTPS 监听均未启用", nil)
	}

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
