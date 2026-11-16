package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/andybalholm/brotli"

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

// contentEncodings 把 Content-Encoding 头拆成依次应用的编码名列表，忽略 identity 与空白。
func contentEncodings(encoding string) []string {
	var names []string
	for _, name := range strings.Split(encoding, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" && name != "identity" {
			names = append(names, name)
		}
	}
	return names
}

// decodeContent 决定 HAR 内容表示：gzip/deflate/br 先解回原文，文本型 MIME 按明文记录，其余（图片、视频等二进制）base64；无法解压时回退为原始字节 base64。size 为解压后内容字节长，供 content.size 记录。
func decodeContent(data []byte, encoding string, mimeType string) (text string, contentEncoding string, size int) {
	if len(data) == 0 {
		return "", "", 0
	}
	if names := contentEncodings(encoding); len(names) > 0 {
		decoded, ok := decompress(data, names)
		if !ok {
			text, contentEncoding = binaryContent(data)
			return text, contentEncoding, len(data)
		}
		data = decoded
	}
	if isTextMimeType(mimeType) {
		return string(data), "", len(data)
	}
	text, contentEncoding = binaryContent(data)
	return text, contentEncoding, len(data)
}

// decompress 按顺序应用 Content-Encoding 解码链，返回解压后字节；遇到未支持编码或流损坏返回 (nil, false)。
func decompress(data []byte, names []string) ([]byte, bool) {
	stream := data
	for _, name := range names {
		switch name {
		case "gzip", "x-gzip":
			out, ok := gunzip(stream)
			if !ok {
				return nil, false
			}
			stream = out
		case "deflate":
			out, ok := inflate(stream)
			if !ok {
				return nil, false
			}
			stream = out
		case "br":
			out, ok := brotliDecode(stream)
			if !ok {
				return nil, false
			}
			stream = out
		default:
			return nil, false
		}
	}
	return stream, true
}

// gunzip 解 gzip 流，读取上限限定内存避免解压炸弹；流被截断时返回已解出部分。
func gunzip(data []byte) ([]byte, bool) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	defer reader.Close()
	out, readErr := io.ReadAll(io.LimitReader(reader, captureBodyLimit))
	return out, readErr == nil || len(out) > 0
}

// inflate 解 deflate：优先按 zlib 头解析，失败回退到 raw deflate（部分服务器误发 RFC 1951 裸流）。
func inflate(data []byte) ([]byte, bool) {
	if reader, err := zlib.NewReader(bytes.NewReader(data)); err == nil {
		defer reader.Close()
		out, readErr := io.ReadAll(io.LimitReader(reader, captureBodyLimit))
		return out, readErr == nil || len(out) > 0
	}
	reader := flate.NewReader(bytes.NewReader(data))
	defer reader.Close()
	out, readErr := io.ReadAll(io.LimitReader(reader, captureBodyLimit))
	return out, readErr == nil || len(out) > 0
}

// brotliDecode 解 br（brotli）流，读取上限限定内存避免解压炸弹。
func brotliDecode(data []byte) ([]byte, bool) {
	out, err := io.ReadAll(io.LimitReader(brotli.NewReader(bytes.NewReader(data)), captureBodyLimit))
	return out, err == nil || len(out) > 0
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
		text, enc, _ := decodeContent(captured.bodyPrefix, request.Header.Get("Content-Encoding"), request.Header.Get("Content-Type"))
		postData = &capture.PostData{
			MimeType:  request.Header.Get("Content-Type"),
			Text:      text,
			Encoding:  enc,
			Truncated: captured.bodyTruncated,
		}
	}
	responseText, responseEnc, responseSize := decodeContent(response.prefix, response.header.Get("Content-Encoding"), contentTypeOf(response.header))
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
			HeadersSize: -1, // 未计算请求头字节数，按 HAR 规范以 -1 表示未知，避免误导性 0
			BodySize:    bodySize,
		},
		Response: capture.Response{
			Status:      response.Status,
			StatusText:  http.StatusText(response.Status),
			HTTPVersion: request.Proto,
			Cookies:     []capture.Cookie{},
			Headers:     headersFrom(response.header),
			Content: capture.Content{
				Size:      responseSize,
				MimeType:  contentTypeOf(response.header),
				Text:      responseText,
				Encoding:  responseEnc,
				Truncated: response.truncated,
			},
			HeadersSize: -1, // 未计算响应头字节数，按 HAR 规范以 -1 表示未知，避免误导性 0
			BodySize:    response.total,
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
