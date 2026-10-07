package main

import (
	"bytes"
	"crypto/tls"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"mrp/internal/dns"
	"mrp/internal/log"
	"mrp/internal/testutil"
)

func init() {
	log.SetOutput(io.Discard)
}

func repoRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

// parseArgs 在隔离的 FlagSet 上解析给定参数，测试间不互相污染
func parseArgs(t *testing.T, args ...string) (startupOptions, error) {
	t.Helper()
	flags := flag.CommandLine
	oldArgs := os.Args
	flag.CommandLine = flag.NewFlagSet("mrp", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"mrp"}, args...)
	t.Cleanup(func() {
		flag.CommandLine = flags
		os.Args = oldArgs
	})
	return parseStartupOptions()
}

func TestParseStartupOptions_Defaults(t *testing.T) {
	opts, err := parseArgs(t)
	if err != nil {
		t.Fatalf("parseStartupOptions: %v", err)
	}
	if opts.port != defaultListenPort {
		t.Fatalf("default port = %d, want %d", opts.port, defaultListenPort)
	}
	if opts.configPath != "config.yaml" || opts.certPath != "ca.crt" || opts.keyPath != "ca.key" {
		t.Fatalf("default paths = %q %q %q", opts.configPath, opts.certPath, opts.keyPath)
	}
	if opts.logLevel != log.InfoLevel {
		t.Fatalf("default log level = %v", opts.logLevel)
	}
	if opts.dnsTimeout != dns.AttemptTimeout || opts.dnsTTL != dns.TTL {
		t.Fatalf("default dns flags = %v %v", opts.dnsTimeout, opts.dnsTTL)
	}
	if opts.configSpecified || opts.certSpecified || opts.keySpecified {
		t.Fatal("unset flags must not count as explicitly specified")
	}
}

func TestParseStartupOptions_Custom(t *testing.T) {
	opts, err := parseArgs(t, "--port", "8080", "--log", "debug", "--dns-ttl", "0",
		"--config", "routes.yaml", "--cert", "my.crt", "--key", "my.key")
	if err != nil {
		t.Fatalf("parseStartupOptions: %v", err)
	}
	if opts.port != 8080 || opts.logLevel != log.DebugLevel || opts.dnsTTL != 0 {
		t.Fatalf("parsed opts = %+v", opts)
	}
	if !opts.configSpecified || !opts.certSpecified || !opts.keySpecified {
		t.Fatalf("specified flags not recorded: %+v", opts)
	}
}

func TestParseStartupOptions_RejectsInvalidValues(t *testing.T) {
	for name, args := range map[string][]string{
		"port_zero":     {"--port", "0"},
		"port_high":     {"--port", "65536"},
		"bad_log_level": {"--log", "verbose"},
		"zero_timeout":  {"--dns-timeout", "0"},
		"negative_ttl":  {"--dns-ttl", "-1s"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseArgs(t, args...); err == nil {
				t.Fatalf("expected error for %v", args)
			}
		})
	}
}

func TestBuildProxy_EnsuresConfigWhenNotSpecified(t *testing.T) {
	opts := startupOptions{
		configPath: filepath.Join(t.TempDir(), "config.yaml"),
		logLevel:   log.ErrorLevel,
		dnsTimeout: dns.AttemptTimeout,
		dnsTTL:     dns.TTL,
	}
	if _, err := buildProxy(opts); err != nil {
		t.Fatalf("buildProxy: %v", err)
	}
	if _, err := os.Stat(opts.configPath); err != nil {
		t.Fatalf("default config was not created: %v", err)
	}
}

func TestBuildProxy_RejectsUnpairedCert(t *testing.T) {
	opts := startupOptions{
		configPath:      filepath.Join(t.TempDir(), "config.yaml"),
		configSpecified: true,
		certPath:        "ca.crt",
		keyPath:         "",
		certSpecified:   true,
		logLevel:        log.ErrorLevel,
		dnsTimeout:      dns.AttemptTimeout,
		dnsTTL:          dns.TTL,
	}
	if _, err := buildProxy(opts); err == nil {
		t.Fatal("unpaired --cert must be rejected")
	}
}

// syncBuffer 供 os/exec 的管道拷贝 goroutine 写入、测试主 goroutine 读取
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func recordingServer(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, tag+":"+request.URL.Path+"|host="+request.Host)
	}))
	t.Cleanup(server.Close)
	return server
}

func buildBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skip e2e build in -short mode")
	}
	name := "mrp"
	// Windows 拒绝执行没有 .exe 后缀的文件，直接启动会报 executable file not found in %PATH%
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/mrp")
	cmd.Dir = repoRoot()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}
	return binary
}

// TestE2E_ConfigNameserversTakeEffect 用真实 mrp 二进制验证配置文件里的 nameservers 字段驱动上游解析
func TestE2E_ConfigNameserversTakeEffect(t *testing.T) {
	binary := buildBinary(t)
	up := recordingServer(t, "upstream")
	_, upstreamPort, err := net.SplitHostPort(up.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	stub := testutil.NewStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	listenPort := testutil.FreePort(t)
	refusedPort := testutil.ClosedUDPPort(t)
	configPath := testutil.ConfigFile(t, "e2e.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: http://api.mrp.local:"+upstreamPort+"\n"+
			"nameservers:\n  - \"127.0.0.1:"+refusedPort+"\"\n  - \""+stub.Address()+"\"\n")
	caCert, caKey := testutil.AuthorityCA(t)
	certPath, keyPath := testutil.WriteCertFiles(t, caCert, caKey)

	var logs syncBuffer
	cmd := exec.Command(binary, "--config", configPath, "--cert", certPath, "--key", keyPath, "--port", listenPort, "--log", "debug")
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	for range 200 {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+listenPort, time.Second)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(testutil.MustURL("http://127.0.0.1:" + listenPort)),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	var body string
	for range 100 {
		request, err := http.NewRequest("GET", "http://api.example.com/hello", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			body = err.Error()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		var buffer bytes.Buffer
		_, _ = io.Copy(&buffer, response.Body)
		_ = response.Body.Close()
		body = buffer.String()
		if strings.HasPrefix(body, "upstream:/hello") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.HasPrefix(body, "upstream:/hello") {
		t.Fatalf("body = %q\nlog:\n%s", body, logs.String())
	}
	if stub.QueryCount() == 0 {
		t.Fatalf("configured nameserver received no dns query\nlog:\n%s", logs.String())
	}
	// 启动日志必须显示配置文件生效的 nameservers，而非 dns.New 的初始默认值
	wantNameservers := "dns nameservers=127.0.0.1:" + refusedPort + "," + stub.Address() + " dns-ttl="
	if !strings.Contains(logs.String(), wantNameservers) {
		t.Fatalf("startup log must show effective nameservers %q\nlog:\n%s", wantNameservers, logs.String())
	}
}

// 配置文件里的 nameservers 在首次 reload 生效，启动日志应打印生效列表而非默认值
func TestBuildProxy_LogsConfiguredNameservers(t *testing.T) {
	stub := testutil.NewStub(t, map[uint16][]net.IP{1: {net.ParseIP("127.0.0.1")}})
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers: []\nnameservers:\n  - \""+stub.Address()+"\"\n")
	opts := startupOptions{
		configPath:      configPath,
		configSpecified: true,
		logLevel:        log.InfoLevel,
		dnsTimeout:      dns.AttemptTimeout,
		dnsTTL:          dns.TTL,
	}
	var out syncBuffer
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	if _, err := buildProxy(opts); err != nil {
		t.Fatalf("buildProxy: %v", err)
	}
	wantNameservers := "dns nameservers=" + stub.Address() + " dns-ttl="
	if !strings.Contains(out.String(), wantNameservers) {
		t.Fatalf("startup log must show configured nameserver %q, got %q", wantNameservers, out.String())
	}
}
