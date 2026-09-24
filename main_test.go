package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func write_config_file(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func self_signed_cert(t *testing.T, domains []string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domains[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     domains,
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func recording_server(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, tag+":"+request.URL.Path+"|host="+request.Host)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func start_proxy(t *testing.T, config_path string, tls_config *tls.Config) (string, *proxy) {
	t.Helper()
	transport_verify := http.DefaultTransport.(*http.Transport).Clone()
	transport_verify.Proxy = nil
	transport_verify.DialContext = (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	transport_verify.ResponseHeaderTimeout = 2 * time.Second
	transport_insecure := transport_verify.Clone()
	transport_insecure.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	p, err := new_proxy(config_path, transport_verify, transport_insecure, tls_config)
	if err != nil {
		t.Fatalf("new_proxy: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() { _ = http.Serve(listener, p) }()
	return "http://" + listener.Addr().String(), p
}

func proxy_client(proxy_url string) *http.Client {
	parsed, _ := url.Parse(proxy_url)
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(parsed),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

func request_body(t *testing.T, client *http.Client, raw_url string) string {
	t.Helper()
	resp, err := client.Get(raw_url)
	if err != nil {
		t.Fatalf("GET %s: %v", raw_url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(body))
}

func TestLoadConfig_Valid(t *testing.T) {
	path := write_config_file(t, "r.yaml", `
servers:
  - domain: a.example.com
    routes:
      - prefix: /v1/
        upstream: http://up-a/v1/
      - prefix: /
        upstream: http://up-a
  - domain: b.example.com
    routes:
      - prefix: /
        upstream: https://up-b
        tls_verify: false
`)
	table, err := load_config(path)
	if err != nil {
		t.Fatalf("load_config: %v", err)
	}
	if len(table.by_domain) != 2 {
		t.Fatalf("want 2 domains, got %d", len(table.by_domain))
	}
	entry, ok := table.pick("a.example.com", "/v1/x")
	if !ok || entry.prefix != "/v1/" {
		t.Fatalf("pick /v1/: got %+v ok=%v", entry, ok)
	}
	if entry, ok := table.pick("b.example.com", "/"); !ok || !entry.skip_verify {
		t.Fatalf("skip_verify not applied: %+v ok=%v", entry, ok)
	}
}

func TestLoadConfig_Errors(t *testing.T) {
	cases := map[string]string{
		"empty_domain": "servers:\n  - domain: \"\"\n    routes: []",
		"dup_domain":   "servers:\n  - domain: a\n    routes: []\n  - domain: a\n    routes: []",
		"empty_prefix": "servers:\n  - domain: a\n    routes:\n      - prefix: \"\"\n        upstream: http://x",
		"dup_prefix":   "servers:\n  - domain: a\n    routes:\n      - prefix: /a\n        upstream: http://x\n      - prefix: /a\n        upstream: http://y",
		"bad_upstream": "servers:\n  - domain: a\n    routes:\n      - prefix: /\n        upstream: ftp://x",
		"bad_yaml":     "servers: [this is broken",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := write_config_file(t, name+".yaml", content)
			if _, err := load_config(path); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestPick_LongestPrefix(t *testing.T) {
	table := &route_table{by_domain: map[string][]*route_entry{
		"a": {
			{prefix: "/"},
			{prefix: "/v1/"},
			{prefix: "/v1/users/"},
		},
	}}
	cases := map[string]string{
		"/v1/users/1": "/v1/users/",
		"/v1/other":   "/v1/",
		"/other":      "/",
	}
	for path, want := range cases {
		entry, ok := table.pick("a", path)
		if !ok || entry.prefix != want {
			t.Fatalf("pick %s: want %s got %+v ok=%v", path, want, entry, ok)
		}
	}
	if _, ok := table.pick("other", "/"); ok {
		t.Fatal("unknown domain should not match")
	}
}

func TestProxy_HTTPRoutingAndPassthrough(t *testing.T) {
	up_a := recording_server(t, "A")
	up_b := recording_server(t, "B")
	content := "servers:\n" +
		"  - domain: api.example.com\n" +
		"    routes:\n" +
		"      - prefix: /v1/\n" +
		"        upstream: " + up_a.URL + "/v1/\n" +
		"      - prefix: /api/\n" +
		"        upstream: " + up_b.URL + "\n" +
		"        host: override.example.com\n"
	cfg := write_config_file(t, "r.yaml", content)
	proxy_url, _ := start_proxy(t, cfg, nil)
	client := proxy_client(proxy_url)

	if got := request_body(t, client, "http://api.example.com/v1/users"); !strings.HasPrefix(got, "A:/v1/users") {
		t.Fatalf("prefix mapping: got %q", got)
	}
	got := request_body(t, client, "http://api.example.com/api/x?q=1")
	if !strings.HasPrefix(got, "B:/x") || !strings.Contains(got, "host=override.example.com") {
		t.Fatalf("host override: got %q", got)
	}
	if got := request_body(t, client, up_a.URL+"/misc"); !strings.HasPrefix(got, "A:/misc") {
		t.Fatalf("passthrough: got %q", got)
	}
}

func TestProxy_HTTPSViaConnect(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, "TLS:"+request.URL.Path)
	}))
	t.Cleanup(up.Close)
	cert := self_signed_cert(t, []string{"api.example.com"})
	tls_config := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12}
	content := "servers:\n" +
		"  - domain: api.example.com\n" +
		"    routes:\n" +
		"      - prefix: /\n" +
		"        upstream: " + up.URL + "\n" +
		"        tls_verify: false\n"
	cfg := write_config_file(t, "r.yaml", content)
	proxy_url, _ := start_proxy(t, cfg, tls_config)
	client := proxy_client(proxy_url)

	got := request_body(t, client, "https://api.example.com/v1/data")
	if got != "TLS:/v1/data" {
		t.Fatalf("https via connect: got %q", got)
	}
}

func TestProxy_502OnUnreachable(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {}))
	closed.Close()
	content := "servers:\n" +
		"  - domain: api.example.com\n" +
		"    routes:\n" +
		"      - prefix: /\n" +
		"        upstream: " + closed.URL + "\n"
	cfg := write_config_file(t, "r.yaml", content)
	proxy_url, _ := start_proxy(t, cfg, nil)
	client := proxy_client(proxy_url)

	resp, err := client.Get("http://api.example.com/x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
}

func TestReload_SwitchesRoute(t *testing.T) {
	up_a := recording_server(t, "A")
	up_b := recording_server(t, "B")
	cfg := write_config_file(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+up_a.URL+"\n")
	proxy_url, p := start_proxy(t, cfg, nil)
	client := proxy_client(proxy_url)

	if got := request_body(t, client, "http://api.example.com/x"); !strings.HasPrefix(got, "A:/x") {
		t.Fatalf("before reload: %q", got)
	}
	_ = os.WriteFile(cfg, []byte(
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+up_b.URL+"\n"), 0o644)
	if err := p.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := request_body(t, client, "http://api.example.com/x"); !strings.HasPrefix(got, "B:/x") {
		t.Fatalf("after reload: %q", got)
	}
}
