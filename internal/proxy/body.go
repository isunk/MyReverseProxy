package proxy

import (
	"bytes"
	"io"
	"net/http"
	"time"

	"mrp/internal/frontend"
	"mrp/internal/log"
)

// captureBodyLimit 单个请求/响应 body 记入 HAR 的最大字节数，超出部分截断。
const captureBodyLimit = 1 << 20

// countingReadCloser 统计从底层读出的字节数，供 Content-Length 未知的请求体在转发完成后回填精确大小。
type countingReadCloser struct {
	io.Reader
	count int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.count += int64(n)
	return n, err
}

// forwardedBody 把合并后的读流包回 io.ReadCloser，Close 转发到底层 body：请求体未完整读完时底层 *http.body 须被关闭以归还连接复用，io.NopCloser 会丢弃该关闭。
type forwardedBody struct {
	io.Reader
	original io.Closer
}

func (b *forwardedBody) Close() error { return b.original.Close() }

// capturedRequest 汇集一条请求的抓包上下文：目标五元组、开始时间与请求体前缀记录。
type capturedRequest struct {
	start         time.Time
	protocol      string
	port          int
	domain        string
	bodyPrefix    []byte
	bodySize      int64
	bodyTruncated bool
	bodyTail      *countingReadCloser
}

func newCapturedRequest(start time.Time, protocol string, port int, domain string, request *http.Request) *capturedRequest {
	prefix, size, truncated, tail := captureRequestBody(request)
	return &capturedRequest{
		start:         start,
		protocol:      protocol,
		port:          port,
		domain:        domain,
		bodyPrefix:    prefix,
		bodySize:      size,
		bodyTruncated: truncated,
		bodyTail:      tail,
	}
}

// captureWriter 在响应回传客户端的同时记录状态码、消息头与 body 前缀，其余能力透传给底层 writer。
type captureWriter struct {
	frontend.StatusRecorder
	wroteHeader bool
	header      http.Header
	prefix      []byte
	total       int64
	truncated   bool
}

func newCaptureWriter(writer http.ResponseWriter) *captureWriter {
	return &captureWriter{StatusRecorder: frontend.StatusRecorder{ResponseWriter: writer, Status: http.StatusOK}}
}

func (w *captureWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.header = w.ResponseWriter.Header().Clone()
	w.StatusRecorder.WriteHeader(code)
}

func (w *captureWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.total += int64(len(data))
	if w.total > captureBodyLimit {
		w.truncated = true
	}
	if len(w.prefix) < captureBodyLimit {
		take := min(captureBodyLimit-len(w.prefix), len(data))
		w.prefix = append(w.prefix, data[:take]...)
	}
	return w.ResponseWriter.Write(data)
}

// captureRequestBody 读取请求 body 前缀并恢复完整 body 供转发：body 超过上限时只记前缀，其余字节经 MultiReader 原样续传；返回前缀、原始大小、是否截断与剩余体计数器（仅 Content-Length 未知时非 nil，供转发完成后回填精确大小）。
func captureRequestBody(request *http.Request) (prefix []byte, size int64, truncated bool, tail *countingReadCloser) {
	if request.Body == nil || request.Body == http.NoBody {
		return nil, request.ContentLength, false, nil
	}
	original := request.Body
	raw, readErr := io.ReadAll(io.LimitReader(original, captureBodyLimit+1))
	if readErr != nil {
		// 客户端中断或连接错误时以已读部分继续转发，仅告警抓包不完整，避免因抓包影响代理主流程
		log.Warn("request body read failed err=%v", readErr)
	}
	truncated = len(raw) > captureBodyLimit
	prefix = raw
	if truncated {
		prefix = raw[:captureBodyLimit]
	}
	if request.ContentLength >= 0 {
		request.Body = &forwardedBody{Reader: io.MultiReader(bytes.NewReader(raw), original), original: original}
		return prefix, request.ContentLength, truncated, nil
	}
	tail = &countingReadCloser{Reader: original}
	request.Body = &forwardedBody{Reader: io.MultiReader(bytes.NewReader(raw), tail), original: original}
	return prefix, int64(len(raw)), truncated, tail
}
