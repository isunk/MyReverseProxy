package capture

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
)

// BodyLimit 单个请求/响应 body 记入 HAR 的最大字节数，超出部分截断。
const BodyLimit = 1 << 20

// HeadersFrom 把 http.Header 展开为 HAR 头列表，同名多值逐条展开。
func HeadersFrom(header http.Header) []Header {
	entries := make([]Header, 0, len(header))
	for name, values := range header {
		for _, value := range values {
			entries = append(entries, Header{Name: name, Value: value})
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

// DecodeContent 决定 HAR 内容表示：gzip/deflate/br 解回原文，文本型 MIME 按明文记录，其余 base64。
func DecodeContent(data []byte, encoding string, mimeType string) (text string, contentEncoding string, size int) {
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

// readLimited 读取 reader 上限 BodyLimit 字节，流损坏或截断时返回已读部分。
func readLimited(reader io.Reader) ([]byte, bool) {
	out, err := io.ReadAll(io.LimitReader(reader, BodyLimit))
	return out, err == nil || len(out) > 0
}

// gunzip 解 gzip 流，读取上限限定内存避免解压炸弹；流被截断时返回已解出部分。
func gunzip(data []byte) ([]byte, bool) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	defer reader.Close()
	return readLimited(reader)
}

// inflate 解 deflate：优先按 zlib 头解析，失败回退到 raw deflate（部分服务器误发 RFC 1951 裸流）。
func inflate(data []byte) ([]byte, bool) {
	if reader, err := zlib.NewReader(bytes.NewReader(data)); err == nil {
		defer reader.Close()
		return readLimited(reader)
	}
	reader := flate.NewReader(bytes.NewReader(data))
	defer reader.Close()
	return readLimited(reader)
}

// brotliDecode 解 br（brotli）流，读取上限限定内存避免解压炸弹。
func brotliDecode(data []byte) ([]byte, bool) {
	return readLimited(brotli.NewReader(bytes.NewReader(data)))
}

// isTextMimeType 判定 MIME 是否属明文文本：text/* 与常见文本型 application/*、SVG 等。
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

// ContentTypeOf 取响应记录到的 Content-Type 头，无记录时返回空串。
func ContentTypeOf(header http.Header) string {
	if header == nil {
		return ""
	}
	return header.Get("Content-Type")
}
