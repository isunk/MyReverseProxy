package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"mrp/internal/ca"
	"mrp/internal/capture"
	"mrp/internal/config"
	"mrp/internal/dns"
	"mrp/internal/frontend"
	"mrp/internal/inspect"
	"mrp/internal/log"
	"mrp/internal/netutil"
	"mrp/internal/proxy"
)

const (
	defaultListenPort = 9000

	// 收到退出信号后等待 in-flight 连接自行结束的时间窗口，超时强制退出；长连接通常不会在此窗口内自然结束。
	shutdownGracePeriod = 10 * time.Second
)

// startupOptions 命令行解析结果，构建代理实例所需的全部启动参数。
type startupOptions struct {
	configPath      string
	port            int
	certPath        string
	keyPath         string
	logLevel        log.Level
	dnsTimeout      time.Duration
	dnsTTL          time.Duration
	captureTarget   string
	configSpecified bool
	certSpecified   bool
	keySpecified    bool
}

// parseStartupOptions 解析并校验命令行参数，非法取值在此集中报错。
func parseStartupOptions() (startupOptions, error) {
	var opts startupOptions
	levelName := ""
	flag.IntVar(&opts.port, "port", defaultListenPort, "single proxy listening port")
	flag.StringVar(&opts.configPath, "config", "mrp.yaml", "routing config file, created automatically when missing")
	flag.StringVar(&opts.certPath, "cert", "ca.crt", "CA certificate file for MITM signing")
	flag.StringVar(&opts.keyPath, "key", "ca.key", "CA private key file")
	flag.StringVar(&levelName, "log", "info", "log level: debug, info, warn or error")
	flag.DurationVar(&opts.dnsTimeout, "dns-timeout", dns.AttemptTimeout, "per-nameserver attempt timeout before failing over to the next")
	flag.DurationVar(&opts.dnsTTL, "dns-ttl", dns.TTL, "ttl of cached upstream resolutions, 0 disables the cache")
	flag.StringVar(&opts.captureTarget, "capture", "", "capture target (default off): host:port exposes a Chrome DevTools target for chrome://inspect, e.g. \":9222\"; a .har path writes an offline HAR file, e.g. \"capture.har\"; any other value is rejected")
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
	if !netutil.ValidPortInt(opts.port) {
		return opts, fmt.Errorf("--port %d: must be 1-65535", opts.port)
	}
	if opts.dnsTimeout <= 0 {
		return opts, fmt.Errorf("--dns-timeout %v: must be > 0", opts.dnsTimeout)
	}
	if opts.dnsTTL < 0 {
		return opts, fmt.Errorf("--dns-ttl %v: must be >= 0", opts.dnsTTL)
	}
	if err := validateCapture(opts.captureTarget); err != nil {
		return opts, fmt.Errorf("invalid --capture %q: %w", opts.captureTarget, err)
	}
	return opts, nil
}

// validateCapture 校验 --capture 取值：只接受 .har 离线文件路径或 host:port 监听地址（仅端口号视为监听所有网卡），其余拒绝。
func validateCapture(value string) error {
	if value == "" || strings.HasSuffix(strings.ToLower(value), ".har") {
		return nil
	}
	port := value
	if _, rest, ok := strings.Cut(value, ":"); ok {
		port = rest
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("listen port %q must be a number from 0 to 65535", port)
	}
	return nil
}

// resolveCapture 解析 --capture 取值：以 .har 结尾（大小写不敏感）按离线 HAR 文件路径处理，其余按 host:port 暴露 Chrome DevTools 目标，
// 仅给端口号时补全为监听所有网卡；返回的两个值恰好一个非空，取值为空表示关闭抓包。取值合法性由 parseStartupOptions 先行校验。
func resolveCapture(value string) (addr string, path string) {
	if value == "" {
		return "", ""
	}
	if strings.HasSuffix(strings.ToLower(value), ".har") {
		return "", value
	}
	if strings.Contains(value, ":") {
		return value, ""
	}
	return ":" + value, ""
}

// buildProxy 组装代理实例：初始化配置与 DNS 服务器、构建上游连接池与路由表；在线抓包开启时附带 CDP 调试端点。
func buildProxy(opts startupOptions) (*proxy.Proxy, *inspect.Server, error) {
	if !opts.configSpecified {
		if err := config.Ensure(opts.configPath); err != nil {
			return nil, nil, fmt.Errorf("ensure config: %w", err)
		}
	}
	if opts.certSpecified != opts.keySpecified {
		return nil, nil, errors.New("--cert and --key must be set together")
	}
	nameservers, err := dns.New(opts.dnsTimeout, opts.dnsTTL, nil)
	if err != nil {
		return nil, nil, err
	}
	transport := proxy.NewTransport(nameservers.DialContext)
	tlsConfig, authority, err := ca.Load(opts.certPath, opts.keyPath, opts.certSpecified)
	if err != nil {
		return nil, nil, fmt.Errorf("load tls config: %w", err)
	}
	proxyInstance, err := proxy.New(opts.configPath, transport, tlsConfig, authority, nameservers)
	if err != nil {
		return nil, nil, err
	}
	captureAddr, capturePath := resolveCapture(opts.captureTarget)
	if capturePath != "" {
		recorder, err := capture.New(capturePath, capture.FileSizeLimit)
		if err != nil {
			log.Warn("capture disabled path=%s err=%v", capturePath, err)
		} else {
			proxyInstance.SetCapture(recorder)
			log.Info("capture started path=%s", capturePath)
		}
	}
	var inspector *inspect.Server
	if captureAddr != "" {
		session := capture.NewSession(capture.MaxEntries, capture.MaxBytes)
		proxyInstance.SetSession(session)
		inspector = inspect.New(session, captureAddr)
	}
	// 首次 reload 已让配置文件里的 nameservers 生效；未配置时为空，按系统默认 DNS 解析。
	log.Info("dns configured nameservers=%s ttl=%s", nameservers.Describe(), opts.dnsTTL)
	return proxyInstance, inspector, nil
}

func main() {
	opts, err := parseStartupOptions()
	if err != nil {
		log.Fatal("flags parse failed err=%v", err)
	}
	log.Init(opts.logLevel)
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", opts.port))
	if err != nil {
		log.Fatal("listen failed addr=:%d err=%v", opts.port, err)
	}
	log.Info("listening addr=:%d", opts.port)
	proxyInstance, inspector, err := buildProxy(opts)
	if err != nil {
		log.Fatal("proxy init failed err=%v", err)
	}
	run(listener, proxyInstance, inspector)
}

// run 在唯一代理端口上常驻服务：监听不随热加载变化，轮询配置并等待信号；退出先关监听器再 drain 连接。
func run(listener net.Listener, proxyInstance *proxy.Proxy, inspector *inspect.Server) {
	var connections sync.WaitGroup
	serveDone := make(chan struct{})
	go func() {
		frontend.Serve(listener, proxyInstance.Handler(), proxyInstance.Socks5(), &connections)
		close(serveDone)
	}()
	// stop 传 nil 表示热加载常驻到进程退出，由下面的退出信号关闭监听来结束。
	go proxyInstance.WatchFile(time.Second, nil)
	if inspector != nil {
		if err := inspector.Start(); err != nil {
			// 调试端点可选，端口被占只降级不终止代理；代理端口监听失败才 Fatal。
			log.Warn("devtools disabled addr=%s err=%v", inspector.Addr(), err)
		} else {
			log.Info("devtools listening addr=%s targets=chrome://inspect", inspector.Addr())
		}
	}
	serveSignals(listener, proxyInstance, &connections, serveDone)
	if inspector != nil {
		_ = inspector.Close()
	}
}

func serveSignals(listener net.Listener, proxyInstance *proxy.Proxy, connections *sync.WaitGroup, serveDone <-chan struct{}) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for sig := range sigCh {
		if sig == syscall.SIGHUP {
			if err := proxyInstance.Reload(); err != nil {
				log.Error("config reload failed err=%v", err)
				continue
			}
			log.Info("config reloaded")
			continue
		}
		log.Info("shutting down")
		if err := listener.Close(); err != nil {
			log.Debug("listener close failed err=%v", err)
		}
		<-serveDone // 等 Serve 退出 accept 循环：保证所有 Add 都已发生在随后的 Wait 之前
		drained := drainConnections(connections, shutdownGracePeriod, sigCh)
		if err := proxyInstance.Close(); err != nil {
			log.Debug("capture close failed err=%v", err)
		}
		if drained {
			log.Info("shutdown complete")
			return
		}
		log.Warn("shutdown timed out, forcing exit")
		return
	}
}

// drainConnections 等待全部活跃连接在宽限期内结束，宽限期用尽或再次收到退出信号返回 false。
func drainConnections(connections *sync.WaitGroup, gracePeriod time.Duration, sigCh <-chan os.Signal) bool {
	drained := make(chan struct{})
	go func() {
		connections.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return true
	case <-time.After(gracePeriod):
		return false
	case <-sigCh:
		return false
	}
}
