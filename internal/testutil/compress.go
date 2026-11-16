package testutil

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"testing"

	"github.com/andybalholm/brotli"
)

// CompressGzip 用 gzip 编码 data，供 Content-Encoding: gzip 的解码路径测试使用。
func CompressGzip(t testing.TB, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	if _, err := gz.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buffer.Bytes()
}

// CompressDeflate 用 zlib（deflate）编码 data，供 Content-Encoding: deflate 的解码路径测试使用。
func CompressDeflate(t testing.TB, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	zw := zlib.NewWriter(&buffer)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buffer.Bytes()
}

// CompressBrotli 用 brotli 编码 data，供 Content-Encoding: br 的解码路径测试使用。
func CompressBrotli(t testing.TB, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	bw := brotli.NewWriter(&buffer)
	if _, err := bw.Write(data); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := bw.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return buffer.Bytes()
}
