package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"mrp/internal/capture"
	"mrp/internal/frontend"
	"mrp/internal/log"
)

// captureBodyLimit 单个请求/响应 body 记入 HAR 的最大字节数，超出部分截断。
const (
	captureBodyLimit = 1 << 20
	harTimeLayout    = "2006-01-02T15:04:05.000Z07:00"
)

// SetCapture 注入抓包记录器，nil 表示关闭抓包；须在开始接受请求前调用一次。
func (p *Proxy) SetCapture(recorder *capture.Recorder) {
	p.capture = recorder
}

// Close 关闭抓包记录器补全 HAR 文件尾部，未开启时为空操作。
func (p *Proxy) Close() error {
	if p.capture == nil {
		return nil
	}
	return p.capture.Close()
}

// capturedRequest 汇集一条请求的抓包上下文：目标五元组、开始时间与请求体前缀记录。
type capturedRequest struct {
	start         time.Time
	protocol      string
	port          int
	domain        string
	bodyPrefix    []byte
	bodySize      int64
	bodyTruncated bool
}

func newCapturedRequest(start time.Time, protocol string, port int, domain string, request *http.Request) *capturedRequest {
	prefix, size, truncated := captureRequestBody(request)
	return &capturedRequest{
		start:         start,
		protocol:      protocol,
		port:          port,
		domain:        domain,
		bodyPrefix:    prefix,
		bodySize:      size,
		bodyTruncated: truncated,
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

// captureRequestBody 读取请求 body 前缀并恢复完整 body 供转发：body 超过上限时只记前缀，其余字节经 MultiReader 原样续传；返回前缀、原始大小与是否截断。
func captureRequestBody(request *http.Request) (prefix []byte, bodySize int64, truncated bool) {
	if request.Body == nil || request.Body == http.NoBody {
		return nil, request.ContentLength, false
	}
	raw, _ := io.ReadAll(io.LimitReader(request.Body, captureBodyLimit+1))
	truncated = len(raw) > captureBodyLimit
	if truncated {
		prefix = raw[:captureBodyLimit]
	} else {
		prefix = raw
	}
	request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), request.Body))
	if request.ContentLength >= 0 {
		return prefix, request.ContentLength, truncated
	}
	return prefix, int64(len(prefix)), truncated
}

// headersFrom 把 http.Header 展开为 HAR 头列表，同名多值逐条展开。
func headersFrom(header http.Header) []capture.Header {
	entries := make([]capture.Header, 0, len(header))
	for name, values := range header {
		for _, value := range values {
			entries = append(entries, capture.Header{Name: name, Value: value})
		}
	}
	return entries
}

// decodeContent 按 Content-Encoding 解压记录体，再按内容判定文本或二进制：gzip/deflate 解为明文，
// br/zstd 等不支持的编码保留原始压缩字节；最后对二进制内容 base64 编码，返回 HAR text 与 encoding 标注。
// 前缀截断导致流不完整时容忍错误，取已解压部分。
func decodeContent(data []byte, encoding string) (string, string) {
	if len(data) == 0 {
		return "", ""
	}
	decoded := data
	if encoding != "" {
		switch enc := strings.ToLower(encoding); {
		case strings.Contains(enc, "gzip"):
			gz, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				return binaryContent(data)
			}
			out, _ := io.ReadAll(gz)
			gz.Close()
			decoded = out
		case strings.Contains(enc, "deflate"):
			reader := flate.NewReader(bytes.NewReader(data))
			out, _ := io.ReadAll(reader)
			reader.Close()
			decoded = out
		default:
			// br/zstd 等不支持编码：decoded 保持原始压缩字节，后续按二进制 base64 处理
		}
	}
	if isTextual(decoded) {
		return string(decoded), ""
	}
	return binaryContent(decoded)
}

// binaryContent 对二进制字节 base64 编码，返回 HAR text 与 encoding 标注。
func binaryContent(data []byte) (string, string) {
	return base64.StdEncoding.EncodeToString(data), "base64"
}

// isTextual 按 NULL 字节与 UTF-8 有效性判定内容是否文本：含 NULL 或非有效 UTF-8 视为二进制。
func isTextual(data []byte) bool {
	if bytes.IndexByte(data, 0) >= 0 {
		return false
	}
	return utf8.Valid(data)
}

// recordCapture 在响应回传后组装一条 HAR entry 并追加；记录失败或达上限时告警一次并停用，不影响转发。
func (p *Proxy) recordCapture(captured *capturedRequest, request *http.Request, response *captureWriter) {
	recorder := p.capture
	if recorder == nil {
		return
	}
	elapsed := float64(time.Since(captured.start).Milliseconds())
	var postData *capture.PostData
	if len(captured.bodyPrefix) > 0 {
		text, enc := decodeContent(captured.bodyPrefix, request.Header.Get("Content-Encoding"))
		postData = &capture.PostData{
			MimeType:  request.Header.Get("Content-Type"),
			Text:      text,
			Encoding:  enc,
			Truncated: captured.bodyTruncated,
		}
	}
	responseText, responseEnc := decodeContent(response.prefix, response.header.Get("Content-Encoding"))
	entry := capture.Entry{
		StartedDateTime: captured.start.UTC().Format(harTimeLayout),
		Time:            elapsed,
		ServerIPAddress: captured.domain,
		ClientAddress:   request.RemoteAddr,
		Request: capture.Request{
			Method:      request.Method,
			URL:         absoluteTarget(captured.protocol, captured.port, captured.domain, request.URL),
			HTTPVersion: request.Proto,
			Cookies:     []capture.Cookie{},
			Headers:     headersFrom(request.Header),
			QueryString: []capture.Query{},
			PostData:    postData,
			BodySize:    captured.bodySize,
		},
		Response: capture.Response{
			Status:      response.Status,
			StatusText:  http.StatusText(response.Status),
			HTTPVersion: request.Proto,
			Cookies:     []capture.Cookie{},
			Headers:     headersFrom(response.header),
			Content: capture.Content{
				Size:      len(response.prefix),
				MimeType:  contentTypeOf(response.header),
				Text:      responseText,
				Encoding:  responseEnc,
				Truncated: response.truncated,
			},
			BodySize: response.total,
		},
		Timings: capture.Timings{Wait: elapsed},
	}
	switch err := recorder.Record(entry); {
	case errors.Is(err, capture.ErrLimitReached):
		log.Warn("capture stopped path=%s: file size limit reached", recorder.Path())
	case err != nil:
		log.Warn("capture stopped path=%s err=%v", recorder.Path(), err)
	}
}

// contentTypeOf 取响应记录到的 Content-Type 头，无记录时返回空串。
func contentTypeOf(header http.Header) string {
	if header == nil {
		return ""
	}
	return header.Get("Content-Type")
}
