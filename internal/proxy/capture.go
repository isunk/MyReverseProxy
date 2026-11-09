package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"time"

	"mrp/internal/capture"
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

// captureWriter 在响应回传客户端的同时记录状态码、消息头与 body 前缀，其余能力透传给底层 writer；Unwrap 保留 flusher 等接口。
type captureWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	header      http.Header
	prefix      []byte
	total       int64
	truncated   bool
}

func newCaptureWriter(writer http.ResponseWriter) *captureWriter {
	return &captureWriter{ResponseWriter: writer, status: http.StatusOK}
}

func (w *captureWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.header = w.ResponseWriter.Header().Clone()
	w.ResponseWriter.WriteHeader(code)
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

func (w *captureWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
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

// recordCapture 在响应回传后组装一条 HAR entry 并追加；记录失败或达上限时告警一次并停用，不影响转发。
func (p *Proxy) recordCapture(start time.Time, protocol string, port int, domain string, request *http.Request, response *captureWriter, requestPrefix []byte, requestSize int64, requestTruncated bool) {
	recorder := p.capture
	if recorder == nil {
		return
	}
	elapsed := float64(time.Since(start).Milliseconds())
	var postData *capture.PostData
	if len(requestPrefix) > 0 {
		postData = &capture.PostData{
			MimeType:  request.Header.Get("Content-Type"),
			Text:      string(requestPrefix),
			Truncated: requestTruncated,
		}
	}
	entry := capture.Entry{
		StartedDateTime: start.UTC().Format(harTimeLayout),
		Time:            elapsed,
		ServerIPAddress: domain,
		ClientAddress:   request.RemoteAddr,
		Request: capture.Request{
			Method:      request.Method,
			URL:         absoluteTarget(protocol, port, domain, request.URL),
			HTTPVersion: request.Proto,
			Cookies:     []capture.Cookie{},
			Headers:     headersFrom(request.Header),
			QueryString: []capture.Query{},
			PostData:    postData,
			BodySize:    requestSize,
		},
		Response: capture.Response{
			Status:      response.status,
			StatusText:  http.StatusText(response.status),
			HTTPVersion: request.Proto,
			Cookies:     []capture.Cookie{},
			Headers:     headersFrom(response.header),
			Content: capture.Content{
				Size:      len(response.prefix),
				MimeType:  contentTypeOf(response.header),
				Text:      string(response.prefix),
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
