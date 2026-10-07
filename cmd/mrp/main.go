package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mrp/internal/ca"
	"mrp/internal/config"
	"mrp/internal/dns"
	"mrp/internal/log"
	"mrp/internal/proxy"
	"mrp/internal/server"
)

const defaultListenPort = 8000

// startupOptions 命令行解析结果，构建代理实例所需的全部启动参数。
type startupOptions struct {
	configPath      string
	port            int
	certPath        string
	keyPath         string
	logLevel        log.Level
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
	flag.IntVar(&opts.port, "port", defaultListenPort, "single proxy listening port")
	flag.StringVar(&opts.configPath, "config", "config.yaml", "routing config file, created automatically when missing")
	flag.StringVar(&opts.certPath, "cert", "ca.crt", "CA certificate file for MITM signing")
	flag.StringVar(&opts.keyPath, "key", "ca.key", "CA private key file")
	flag.StringVar(&levelName, "log", "info", "log level: debug, info, warn or error")
	flag.DurationVar(&opts.dnsTimeout, "dns-timeout", dns.AttemptTimeout, "per-nameserver attempt timeout before failing over to the next")
	flag.DurationVar(&opts.dnsTTL, "dns-ttl", dns.TTL, "ttl of cached upstream resolutions, 0 disables the cache")
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
	level, err := log.ParseLevel(levelName)
	if err != nil {
		return opts, err
	}
	opts.logLevel = level
	if opts.port < 1 || opts.port > 65535 {
		return opts, fmt.Errorf("invalid --port %d: must be between 1 and 65535", opts.port)
	}
	if opts.dnsTimeout <= 0 {
		return opts, fmt.Errorf("invalid --dns-timeout %v: must be positive", opts.dnsTimeout)
	}
	if opts.dnsTTL < 0 {
		return opts, fmt.Errorf("invalid --dns-ttl %v: must be zero or positive", opts.dnsTTL)
	}
	return opts, nil
}

// buildProxy 组装代理实例：初始化配置与 DNS 服务器、构建上游连接池与路由表。
func buildProxy(opts startupOptions) (*proxy.Proxy, error) {
	if !opts.configSpecified {
		if err := config.Ensure(opts.configPath); err != nil {
			return nil, fmt.Errorf("initialize routing config: %w", err)
		}
	}
	if opts.certSpecified != opts.keySpecified {
		return nil, errors.New("--cert and --key must be set together")
	}
	nameservers, err := dns.New(opts.dnsTimeout, opts.dnsTTL, dns.DefaultNameservers)
	if err != nil {
		return nil, fmt.Errorf("initialize dns nameservers: %w", err)
	}
	transport := proxy.NewTransport(nameservers.DialContext)
	tlsConfig, authority, err := ca.Load(opts.certPath, opts.keyPath, opts.certSpecified)
	if err != nil {
		return nil, fmt.Errorf("tls certificate configuration: %w", err)
	}
	proxyInstance, err := proxy.New(opts.configPath, transport, tlsConfig, authority, nameservers)
	if err != nil {
		return nil, err
	}
	// 首次 reload 已让配置文件里的 nameservers 生效，此处打印实际生效列表而非初始默认值
	log.Info("dns nameservers=%s dns-ttl=%s", strings.Join(nameservers.Addresses(), ","), opts.dnsTTL)
	return proxyInstance, nil
}

func main() {
	opts, err := parseStartupOptions()
	if err != nil {
		log.Fatal("invalid flags: %v", err)
	}
	log.Init(opts.logLevel)
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", opts.port))
	if err != nil {
		log.Fatal("listen addr=:%d failed: %v", opts.port, err)
	}
	log.Info("listening addr=:%d", opts.port)
	proxyInstance, err := buildProxy(opts)
	if err != nil {
		log.Fatal("startup failed: %v", err)
	}
	run(listener, proxyInstance)
}

// run 在唯一代理端口上常驻服务：监听不随配置热加载变化，
// 此后仅轮询配置热加载并等待信号。退出信号关闭监听后返回。
func run(listener net.Listener, proxyInstance *proxy.Proxy) {
	go server.Serve(listener, proxyInstance.Handler())
	// stop 传 nil 表示热加载常驻到进程退出，由上面的退出信号关闭监听来结束
	go proxyInstance.WatchFile(time.Second, nil)
	serveSignals(listener, proxyInstance)
}

func serveSignals(listener net.Listener, proxyInstance *proxy.Proxy) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for sig := range sigCh {
		if sig != syscall.SIGHUP {
			listener.Close()
			return
		}
		if err := proxyInstance.Reload(); err != nil {
			log.Error("hot reload failed, keeping current config: %v", err)
			continue
		}
		log.Info("routing config reloaded")
	}
}
