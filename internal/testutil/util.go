package testutil

import (
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// ConfigFile 在临时目录写入文件，返回绝对路径，随测试结束自动清理。
func ConfigFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// FreePort 返回一个当前可用的 TCP 端口。
func FreePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// EchoServer 启动原样回显的 TCP 服务，返回监听地址。
func EchoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) { io.Copy(conn, conn); conn.Close() }(conn)
		}
	}()
	return listener.Addr().String()
}

// MustURL 解析 URL，失败即 panic，测试里省去错误分支。
func MustURL(value string) *url.URL {
	parsed, err := url.Parse(value)
	if err != nil {
		panic(err)
	}
	return parsed
}
