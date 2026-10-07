package server

import (
	"bufio"
	"errors"
	stdlog "log"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"mrp/internal/log"
)

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 60 * time.Second

	acceptBackoffMax = time.Second
)

// oneConnListener 把已建立的连接包装为 Listener，让 http.Server 直接服务该连接，
// 从而拿到 ConnState 回调判断是否发生 hijack。
type oneConnListener struct {
	conn  net.Conn
	taken atomic.Bool
	done  chan struct{}
}

func newOneConnListener(conn net.Conn) *oneConnListener {
	return &oneConnListener{conn: conn, done: make(chan struct{})}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.taken.CompareAndSwap(false, true) {
		return l.conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) finish() {
	close(l.done)
}

func (l *oneConnListener) Close() error { return nil }

func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// BufferedConn 把 http.Server 内部的 bufio.Reader 预读数据接回连接，
// 供 hijack 后读取紧跟 CONNECT 的字节。
type BufferedConn struct {
	net.Conn
	Reader *bufio.Reader
}

func (c *BufferedConn) Read(p []byte) (int, error) { return c.Reader.Read(p) }

// Serve 接受连接并逐条服务：单条连接服务结束（未发生 hijack）时关闭该连接。
// 监听器已关闭即返回；其余 Accept 错误退避重试，设备上 fd 短暂耗尽（EMFILE）
// 属临时故障，停止重试会让代理在进程存活的情况下静默拒绝一切连接。
func Serve(listener net.Listener, handler http.Handler) error {
	backoff := 5 * time.Millisecond
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			log.Error("listener accept failed, retrying in %s: %v", backoff, err)
			time.Sleep(backoff)
			backoff = min(backoff*2, acceptBackoffMax)
			continue
		}
		backoff = 5 * time.Millisecond
		go func() {
			if !ServeSingleConn(New(handler), conn) {
				conn.Close()
			}
		}()
	}
}

// ServeSingleConn 在单条连接上跑一次 http.Server.Serve，返回是否发生过 hijack
func ServeSingleConn(server *http.Server, conn net.Conn) bool {
	listener := newOneConnListener(conn)
	finish := sync.OnceFunc(listener.finish)
	var hijacked atomic.Bool
	server.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateHijacked {
			hijacked.Store(true)
		}
		if state == http.StateClosed || state == http.StateHijacked {
			finish()
		}
	}
	_ = server.Serve(listener)
	return hijacked.Load()
}

var httpErrorLog = stdlog.New(os.Stderr, "", 0)

// New 构建承载单条连接的 http.Server：统一读头超时与空闲超时。
func New(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// 去掉标准 log 的日期前缀，底层错误按各自级别落到 stderr，不与控制台日志格式混用
		ErrorLog: httpErrorLog,
	}
}

// StatusRecorder 记录响应状态码供请求日志使用，其余能力透传给底层 writer；
// 提供 Unwrap，使包装链上的 flusher/hijacker 等接口继续可用。
type StatusRecorder struct {
	http.ResponseWriter
	Status int
}

func (w *StatusRecorder) WriteHeader(code int) {
	w.Status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *StatusRecorder) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
