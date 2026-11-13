package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mrp/internal/ca"
	"mrp/internal/capture"
	"mrp/internal/testutil"
)

func captureFilePath(t *testing.T) string {
	return filepath.Join(t.TempDir(), "capture.har")
}

func readHar(t *testing.T, path string) capture.Har {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var har capture.Har
	if err := json.Unmarshal(data, &har); err != nil {
		t.Fatalf("unmarshal HAR: %v", err)
	}
	return har
}

func startCapturedProxy(t *testing.T, configPath, capturePath string) (string, *capture.Recorder) {
	t.Helper()
	servers := testNameservers(t)
	proxyInstance, err := New(configPath, NewTransport(servers.DialContext), nil, nil, servers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	recorder, err := capture.New(capturePath, capture.FileSizeLimit)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	proxyInstance.SetCapture(recorder)
	return startListener(t, proxyInstance), recorder
}

func TestCapture_RecordsPlainHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Add("X-Multi", "one")
		writer.Header().Add("X-Multi", "two")
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(writer, "hello capture")
	}))
	t.Cleanup(upstream.Close)
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upstream.URL+"\n")
	path := captureFilePath(t)
	proxyURL, recorder := startCapturedProxy(t, configPath, path)
	client := proxyClient(proxyURL)

	if got := requestBody(t, client, "http://api.example.com/hello"); !strings.Contains(got, "hello capture") {
		t.Fatalf("proxy response: %q", got)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	har := readHar(t, path)
	if len(har.Log.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(har.Log.Entries))
	}
	entry := har.Log.Entries[0]
	if entry.Request.Method != http.MethodGet {
		t.Fatalf("method = %q, want GET", entry.Request.Method)
	}
	if !strings.Contains(entry.Request.URL, "api.example.com") || !strings.Contains(entry.Request.URL, "/hello") {
		t.Fatalf("url = %q, want to contain api.example.com and /hello", entry.Request.URL)
	}
	if entry.Response.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", entry.Response.Status)
	}
	if !strings.Contains(entry.Response.Content.Text, "hello capture") {
		t.Fatalf("response text = %q", entry.Response.Content.Text)
	}
	if entry.Response.Content.MimeType == "" {
		t.Fatalf("mimeType should be captured from Content-Type")
	}
	multi := 0
	for _, header := range entry.Response.Headers {
		if header.Name == "X-Multi" {
			multi++
		}
	}
	if multi != 2 {
		t.Fatalf("X-Multi header occurrences = %d, want 2", multi)
	}
	if entry.ClientAddress == "" {
		t.Fatalf("client address should be recorded")
	}
	if entry.ServerName != "api.example.com" {
		t.Fatalf("serverName = %q, want api.example.com", entry.ServerName)
	}
}

func TestCapture_TruncatesLargeBody(t *testing.T) {
	payload := strings.Repeat("x", captureBodyLimit+64)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, payload)
	}))
	t.Cleanup(upstream.Close)
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upstream.URL+"\n")
	path := captureFilePath(t)
	proxyURL, recorder := startCapturedProxy(t, configPath, path)
	client := proxyClient(proxyURL)

	resp, err := client.Get("http://api.example.com/large")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if int64(len(body)) != int64(len(payload)) {
		t.Fatalf("forwarded body len = %d, want %d (must not be truncated)", len(body), len(payload))
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	har := readHar(t, path)
	entry := har.Log.Entries[0]
	if !entry.Response.Content.Truncated {
		t.Fatalf("large response should be marked truncated")
	}
	if entry.Response.Content.Size != captureBodyLimit {
		t.Fatalf("content size = %d, want %d", entry.Response.Content.Size, captureBodyLimit)
	}
	if int64(len(entry.Response.Content.Text)) != captureBodyLimit {
		t.Fatalf("recorded text len = %d, want %d", len(entry.Response.Content.Text), captureBodyLimit)
	}
}

// MITM 解密后的内层请求复用 serveRequest，应被记录；CONNECT 建立本身不产生 entry
func TestCapture_MITMInnerRecorded(t *testing.T) {
	upstream := recordingServer(t, "wild")
	caCert, caKey := testutil.AuthorityCA(t)
	authority := ca.New(caCert, caKey)
	tlsConfig := &tls.Config{
		GetCertificate: authority.GetCertificate,
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: \"*\"\n    routes:\n      - prefix: /\n        upstream: "+upstream.URL+"\n")
	path := captureFilePath(t)
	servers := testNameservers(t)
	proxyInstance, err := New(configPath, NewTransport(servers.DialContext), tlsConfig, authority, servers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	recorder, err := capture.New(path, capture.FileSizeLimit)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	proxyInstance.SetCapture(recorder)
	proxyURL := startListener(t, proxyInstance)
	addr := strings.TrimPrefix(proxyURL, "http://")

	proxyConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer proxyConn.Close()
	fmt.Fprintf(proxyConn, "CONNECT test.example.org:443 HTTP/1.1\r\nHost: test.example.org:443\r\n\r\n")
	if code := readConnectResponse(t, bufio.NewReader(proxyConn)); code != 200 {
		t.Fatalf("CONNECT: want 200, got %d", code)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	innerConn := tls.Client(proxyConn, &tls.Config{RootCAs: roots, ServerName: "test.example.org"})
	if err := innerConn.HandshakeContext(t.Context()); err != nil {
		t.Fatalf("inner handshake: %v", err)
	}
	fmt.Fprintf(innerConn, "GET /inner-path HTTP/1.1\r\nHost: test.example.org\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(innerConn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read inner response: %v", err)
	}
	resp.Body.Close()
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	har := readHar(t, path)
	if len(har.Log.Entries) != 1 {
		t.Fatalf("entries = %d, want 1 (inner MITM request only)", len(har.Log.Entries))
	}
	if !strings.Contains(har.Log.Entries[0].Request.URL, "test.example.org") {
		t.Fatalf("url = %q, want test.example.org", har.Log.Entries[0].Request.URL)
	}
}

// tunnel 流量是端到端加密字节流，走字节搬运不经 serveRequest，不应产生 entry
func TestCapture_TunnelNotRecorded(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {}))
	closed.Close()
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: placeholder.example.com\n    routes:\n      - prefix: /\n        upstream: http://127.0.0.1:1\n")
	path := captureFilePath(t)
	proxyURL, recorder := startCapturedProxy(t, configPath, path)

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	target := strings.TrimPrefix(closed.URL, "http://")
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	if code := readConnectResponse(t, bufio.NewReader(conn)); code != 502 {
		t.Fatalf("want 502 from failed tunnel dial, got %d", code)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	har := readHar(t, path)
	if len(har.Log.Entries) != 0 {
		t.Fatalf("entries = %d, want 0 (tunnel must not be recorded)", len(har.Log.Entries))
	}
}

// 上游返回 gzip 编码的响应时，HAR 的 content.text 应记录解压后的明文，而非压缩字节乱码；原始 Content-Encoding 头仍保留在 headers
func TestCapture_GzipResponseDecoded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Encoding", "gzip")
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		gz := gzip.NewWriter(writer)
		io.WriteString(gz, "hello gzip world")
		gz.Close()
	}))
	t.Cleanup(upstream.Close)
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upstream.URL+"\n")
	path := captureFilePath(t)
	proxyURL, recorder := startCapturedProxy(t, configPath, path)
	client := proxyClient(proxyURL)

	if got := requestBody(t, client, "http://api.example.com/gz"); !strings.Contains(got, "hello gzip world") {
		t.Fatalf("client should receive decoded body, got %q", got)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	har := readHar(t, path)
	entry := har.Log.Entries[0]
	if entry.Response.Content.Text != "hello gzip world" {
		t.Fatalf("content.text = %q, want decoded plaintext", entry.Response.Content.Text)
	}
	found := false
	for _, header := range entry.Response.Headers {
		if header.Name == "Content-Encoding" && header.Value == "gzip" {
			found = true
		}
	}
	if !found {
		t.Fatalf("original Content-Encoding header should be recorded")
	}
}

// 二进制响应（如图片）应以 base64 编码记入 HAR content.text，encoding 标注 "base64"，而非原始字节乱码
func TestCapture_BinaryResponseBase64(t *testing.T) {
	body := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01")
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "image/png")
		writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = writer.Write(body)
	}))
	t.Cleanup(upstream.Close)
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upstream.URL+"\n")
	path := captureFilePath(t)
	proxyURL, recorder := startCapturedProxy(t, configPath, path)
	client := proxyClient(proxyURL)

	resp, err := client.Get("http://api.example.com/img")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("client should receive raw binary, got %d bytes want %d", len(got), len(body))
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	har := readHar(t, path)
	entry := har.Log.Entries[0]
	if entry.Response.Content.Encoding != "base64" {
		t.Fatalf("encoding = %q, want base64", entry.Response.Content.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(entry.Response.Content.Text)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if !bytes.Equal(decoded, body) {
		t.Fatalf("decoded base64 content != original body")
	}
}

// 截断的 gzip 前缀（超出 body 上限）应容忍解压失败，保留已解压部分而非记录压缩乱码
func TestCapture_GzipTruncatedDecoded(t *testing.T) {
	plain := strings.Repeat("hello ", 3*captureBodyLimit/len("hello "))
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	io.WriteString(gz, plain)
	gz.Close()
	encoded := buffer.Bytes()

	if text, enc := decodeContent(encoded, "gzip"); text != plain || enc != "" {
		t.Fatalf("decodeContent(gzip) = (%q, %q), want full plaintext no encoding", text, enc)
	}
	if text, _ := decodeContent(encoded[:len(encoded)/2], "gzip"); !strings.HasPrefix(plain, text) || text == "" {
		t.Fatalf("truncated gzip should decode available prefix, got %d bytes", len(text))
	}
	if text, enc := decodeContent(encoded, "br"); enc != "base64" {
		t.Fatalf("unknown encoding should base64 binary content, got encoding=%q text=%d bytes", enc, len(text))
	}
	if text, enc := decodeContent([]byte("plain"), ""); text != "plain" || enc != "" {
		t.Fatalf("no encoding should keep raw text, got (%q, %q)", text, enc)
	}
}

// 不支持编码的内容即使字节恰为有效 UTF-8（无 NULL）也按二进制 base64 处理，不得当文本。
func TestCapture_UnsupportedEncodingNeverText(t *testing.T) {
	// 有效 UTF-8 且无 NULL 的字节序列，覆盖旧逻辑误判为文本的分支
	asciiLike := []byte("Brotli compressed stream that looks like text without null bytes")
	if !isTextual(asciiLike) {
		t.Fatalf("test fixture should be textual to exercise the bug")
	}
	text, enc := decodeContent(asciiLike, "br")
	if enc != "base64" {
		t.Fatalf("decodeContent(br-like) encoding = %q, want base64 (never text)", enc)
	}
	decoded, err := base64.StdEncoding.DecodeString(text)
	if err != nil || !bytes.Equal(decoded, asciiLike) {
		t.Fatalf("decodeContent(br-like) text should be base64 of original bytes, got %d bytes err=%v", len(decoded), err)
	}
}

// 无 Content-Length 的 chunked 请求体超过记录上限时，bodySize 应在转发完成后回填为精确大小，而非前缀上限长度
func TestCapture_ChunkedBodySizeAccurate(t *testing.T) {
	payload := strings.Repeat("y", captureBodyLimit+4096)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		n, _ := io.Copy(io.Discard, request.Body)
		io.WriteString(writer, strconv.FormatInt(n, 10))
	}))
	t.Cleanup(upstream.Close)
	configPath := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: "+upstream.URL+"\n")
	path := captureFilePath(t)
	proxyURL, recorder := startCapturedProxy(t, configPath, path)
	client := proxyClient(proxyURL)

	// 用 NopCloser 包住 *strings.Reader 使客户端无法推断长度，强制走 chunked 传输
	resp, err := client.Post("http://api.example.com/upload", "application/octet-stream", io.NopCloser(strings.NewReader(payload)))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	received, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(received) != strconv.Itoa(len(payload)) {
		t.Fatalf("upstream received %s bytes, want %d", received, len(payload))
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	har := readHar(t, path)
	entry := har.Log.Entries[0]
	if entry.Request.BodySize != int64(len(payload)) {
		t.Fatalf("bodySize = %d, want %d", entry.Request.BodySize, len(payload))
	}
	if entry.Request.PostData == nil || !entry.Request.PostData.Truncated {
		t.Fatalf("large chunked body should be marked truncated")
	}
}
