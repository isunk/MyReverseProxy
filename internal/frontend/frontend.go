package frontend

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

	acceptBackoffInitial = 5 * time.Millisecond
	acceptBackoffMax     = time.Second
)

// oneConnListener 把已建立的连接包装为 Listener，让 http.Server 直接服务该连接，从而拿到 ConnState 回调判断是否发生 hijack。
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

// BufferedConn 把 http.Server 内部的 bufio.Reader 预读数据接回连接，供 hijack 后读取紧跟 CONNECT 的字节。
type BufferedConn struct {
	net.Conn
	Reader *bufio.Reader
}

func (c *BufferedConn) Read(p []byte) (int, error) { return c.Reader.Read(p) }

// socks5Version 是 SOCKS5 协议版本字节，用于在单端口上从连接首字节区分 SOCKS5 与 HTTP 请求。
const socks5Version = 0x05

// Serve 接受连接并逐条服务：单条连接服务结束（未发生 hijack）时关闭该连接；监听器已关闭即返回，其余 Accept 错误退避重试，避免 fd 短暂耗尽（EMFILE）等临时故障让代理静默拒绝连接；connections 为在服务连接计数以供退出前 drain。
func Serve(listener net.Listener, handler http.Handler, socks5 func(net.Conn), connections *sync.WaitGroup) error {
	backoff := acceptBackoffInitial
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			log.Error("accept failed retry=%s err=%v", backoff, err)
			time.Sleep(backoff)
			backoff = min(backoff*2, acceptBackoffMax)
			continue
		}
		backoff = acceptBackoffInitial
		connections.Add(1)
		go func() {
			defer connections.Done()
			serveConn(handler, socks5, conn)
		}()
	}
}

// serveConn 嗅探连接首字节选择协议：为 SOCKS5 版本号时交给 SOCKS5 处理器，否则按 HTTP 服务；未发生 hijack 的 HTTP 连接在此关闭。嗅探读首字节设 readHeaderTimeout 超时，避免慢速连接在进入 http.Server 前永久占用 goroutine。
func serveConn(handler http.Handler, socks5 func(net.Conn), conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(readHeaderTimeout))
	buffered := bufio.NewReader(conn)
	first, err := buffered.Peek(1)
	if err != nil {
		conn.Close()
		return
	}
	if first[0] == socks5Version && socks5 != nil {
		_ = conn.SetReadDeadline(time.Time{})
		socks5(&BufferedConn{Conn: conn, Reader: buffered})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	// ServeSingleConn 会给传入 server 的 ConnState 赋值，复用同一实例会在并发连接间相互覆盖，故每条连接新建。
	if !ServeSingleConn(New(handler), &BufferedConn{Conn: conn, Reader: buffered}) {
		conn.Close()
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

// StatusRecorder 记录响应状态码供请求日志使用，其余能力透传给底层 writer；Unwrap 使 flusher/hijacker 等接口继续可用。
type StatusRecorder struct {
	http.ResponseWriter
	Status int
}

func (w *StatusRecorder) WriteHeader(code int) {
	w.Status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush 转发到底层，使流式响应（如 SSE、长轮询）的 Flusher 能力不被包装层遮蔽。
func (w *StatusRecorder) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *StatusRecorder) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
