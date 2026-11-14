package proxy

import (
	"encoding/base64"
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"

	"mrp/internal/capture"
	"mrp/internal/log"
)

// harTimeLayout HAR 1.2 时间戳格式。
const harTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// SetCapture 注入抓包记录器，nil 表示关闭抓包；须在开始接受请求前调用一次。
func (p *Proxy) SetCapture(recorder *capture.Recorder) {
	p.capture.Store(recorder)
}

// Close 关闭抓包记录器补全 HAR 文件尾部，未开启时为空操作。
func (p *Proxy) Close() error {
	if recorder := p.capture.Load(); recorder != nil {
		return recorder.Close()
	}
	return nil
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

// decodeContent 按 MIME 类型与 Content-Encoding 决定 HAR 内容表示：明文文本（无编码的文本型 MIME）按原文记录，其余（压缩或二进制）一律 base64，不做解压、不篡改 content-type 与 size。
func decodeContent(data []byte, encoding string, mimeType string) (string, string) {
	if len(data) == 0 {
		return "", ""
	}
	if (encoding == "" || strings.EqualFold(encoding, "identity")) && isTextMimeType(mimeType) {
		return string(data), ""
	}
	return binaryContent(data)
}

// isTextMimeType 判定 MIME 类型是否属于明文文本：text/* 与常见文本型 application/*、SVG 等；用于决定 HAR content 按明文还是 base64 记录。
func isTextMimeType(mimeType string) bool {
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		mediaType = strings.TrimSpace(mimeType)
	}
	mediaType = strings.ToLower(mediaType)
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/json", "application/ld+json", "application/x-ndjson",
		"application/javascript", "application/x-javascript", "application/ecmascript",
		"application/xml", "application/xhtml+xml", "application/x-www-form-urlencoded",
		"application/graphql", "application/manifest+json", "application/sql",
		"image/svg+xml":
		return true
	}
	return false
}

// binaryContent 对二进制字节 base64 编码，返回 HAR text 与 encoding 标注。
func binaryContent(data []byte) (string, string) {
	return base64.StdEncoding.EncodeToString(data), "base64"
}

// recordCapture 在响应回传后组装一条 HAR entry 并追加；记录失败或达上限时告警一次并停用，不影响转发。
func (p *Proxy) recordCapture(captured *capturedRequest, request *http.Request, response *captureWriter) {
	recorder := p.capture.Load()
	if recorder == nil {
		return
	}
	elapsed := float64(time.Since(captured.start).Milliseconds())
	bodySize := captured.bodySize
	if captured.bodyTail != nil {
		bodySize += captured.bodyTail.count
	}
	var postData *capture.PostData
	if len(captured.bodyPrefix) > 0 {
		text, enc := decodeContent(captured.bodyPrefix, request.Header.Get("Content-Encoding"), request.Header.Get("Content-Type"))
		postData = &capture.PostData{
			MimeType:  request.Header.Get("Content-Type"),
			Text:      text,
			Encoding:  enc,
			Truncated: captured.bodyTruncated,
		}
	}
	responseText, responseEnc := decodeContent(response.prefix, response.header.Get("Content-Encoding"), contentTypeOf(response.header))
	entry := capture.Entry{
		StartedDateTime: captured.start.UTC().Format(harTimeLayout),
		Time:            elapsed,
		ServerName:      captured.domain,
		ClientAddress:   request.RemoteAddr,
		Request: capture.Request{
			Method:      request.Method,
			URL:         absoluteTarget(captured.protocol, captured.port, captured.domain, request.URL),
			HTTPVersion: request.Proto,
			Cookies:     []capture.Cookie{},
			Headers:     headersFrom(request.Header),
			QueryString: []capture.Query{},
			PostData:    postData,
			BodySize:    bodySize,
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
