package capture

import (
	"bytes"
	"encoding/base64"
	"testing"

	"mrp/internal/testutil"
)

// DecodeContent 行为规则：无编码文本按原文，gzip/deflate/br 解回原文后按 MIME 归文本或 base64，二进制 MIME 或不可解码字节 base64。
func TestDecodeContent_MimeRule(t *testing.T) {
	if text, enc, _ := DecodeContent([]byte("plain"), "", "text/plain"); text != "plain" || enc != "" {
		t.Fatalf("DecodeContent(text/plain) = (%q, %q), want plaintext no encoding", text, enc)
	}
	if text, enc, _ := DecodeContent([]byte(`{"a":1}`), "", "application/json"); text != `{"a":1}` || enc != "" {
		t.Fatalf("DecodeContent(application/json) = (%q, %q), want plaintext no encoding", text, enc)
	}
	if _, enc, _ := DecodeContent([]byte("\x89PNG\x00"), "", "image/png"); enc != "base64" {
		t.Fatalf("DecodeContent(image/png) encoding = %q, want base64 (binary mime)", enc)
	}

	if text, enc, size := DecodeContent(testutil.CompressGzip(t, []byte("gzip hello")), "gzip", "text/plain"); text != "gzip hello" || enc != "" || size != len("gzip hello") {
		t.Fatalf("DecodeContent(gzip text) = (%q, %q, %d), want decoded plaintext", text, enc, size)
	}
	if text, enc, _ := DecodeContent(testutil.CompressDeflate(t, []byte("deflate hello")), "deflate", "text/plain"); text != "deflate hello" || enc != "" {
		t.Fatalf("DecodeContent(deflate text) = (%q, %q), want decoded plaintext", text, enc)
	}
	if text, enc, _ := DecodeContent(testutil.CompressBrotli(t, []byte("brotli hello")), "br", "text/plain"); text != "brotli hello" || enc != "" {
		t.Fatalf("DecodeContent(br text) = (%q, %q), want decoded plaintext", text, enc)
	}

	// 压缩的二进制内容解压后仍按 base64 记录
	if text, enc, _ := DecodeContent(testutil.CompressGzip(t, []byte("\x89PNG\x00")), "gzip", "image/png"); enc != "base64" {
		t.Fatalf("DecodeContent(gzip image) encoding = %q, want base64", enc)
	} else if decoded, err := base64.StdEncoding.DecodeString(text); err != nil || !bytes.Equal(decoded, []byte("\x89PNG\x00")) {
		t.Fatalf("DecodeContent(gzip image) should base64 the decompressed bytes, got %d bytes err=%v", len(decoded), err)
	}

	// 声称 gzip/br 但流无法解码时回退为原始字节 base64，不当文本
	for _, tc := range []struct {
		encoding string
		data     []byte
	}{
		{"gzip", []byte("not gzip data")},
		{"br", []byte("Brotli-like text without null bytes")},
	} {
		if _, enc, _ := DecodeContent(tc.data, tc.encoding, "text/plain"); enc != "base64" {
			t.Fatalf("DecodeContent(invalid %s) encoding = %q, want base64 fallback", tc.encoding, enc)
		}
	}
}
