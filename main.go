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
	http_listen  string
	https_listen string
)

func main() {
	config_path := flag.String("config", "routing.yaml", "路由配置文件路径")
	http_addr := flag.String("http", ":80", "HTTP 监听地址，传空禁用")
	https_addr := flag.String("https", ":443", "HTTPS 监听地址，传空禁用")
	tls_cert := flag.String("tls-cert", "", "TLS 证书路径，启用 HTTPS 时必填")
	tls_key := flag.String("tls-key", "", "TLS 私钥路径，启用 HTTPS 时必填")
	log_level := flag.String("log-level", "info", "日志级别 debug/info/warn/error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*log_level)); err != nil {
		fatal("非法日志级别 "+*log_level, err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	http_listen = *http_addr
	https_listen = *https_addr

	transport_verify := http.DefaultTransport.(*http.Transport).Clone()
	transport_verify.Proxy = nil
	transport_verify.DialContext = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	transport_verify.ResponseHeaderTimeout = 30 * time.Second
	transport_insecure := transport_verify.Clone()
	transport_insecure.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	var tls_config *tls.Config
	if https_listen != "" {
		if *tls_cert == "" || *tls_key == "" {
			fatal("启用 HTTPS 监听时必须提供 --tls-cert 与 --tls-key", nil)
		}
		cert, err := tls.LoadX509KeyPair(*tls_cert, *tls_key)
		if err != nil {
			fatal("加载 TLS 证书失败", err)
		}
		tls_config = &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"http/1.1"},
			MinVersion:   tls.VersionTLS12,
		}
	}

	proxy_instance, err := new_proxy(*config_path, transport_verify, transport_insecure, tls_config)
	if err != nil {
		fatal("加载路由配置失败", err)
	}
	run(proxy_instance)
}

func run(p *proxy) {
	if http_listen != "" {
		go func() {
			slog.Info("HTTP 监听", "addr", http_listen)
			if err := http.ListenAndServe(http_listen, p); err != nil {
				fatal("HTTP 监听失败", err)
			}
		}()
	}
	if https_listen != "" {
		if p.tls_config == nil {
			fatal("启用 HTTPS 监听时必须提供 --tls-cert 与 --tls-key", nil)
		}
		listener, err := net.Listen("tcp", https_listen)
		if err != nil {
			fatal("HTTPS 监听失败", err)
		}
		slog.Info("HTTPS 监听", "addr", https_listen)
		go func() {
			if err := serve_https(listener, p.tls_config, p); err != nil {
				fatal("HTTPS 服务退出", err)
			}
		}()
	}
	if http_listen == "" && https_listen == "" {
		fatal("HTTP 与 HTTPS 监听均未启用", nil)
	}

	signal_channel := make(chan os.Signal, 1)
	signal.Notify(signal_channel, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for signal_received := range signal_channel {
		if signal_received == syscall.SIGHUP {
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
