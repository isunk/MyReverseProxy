// Package netutil 提供跨包复用的网络基础组件：拷贝缓冲池、双向字节搬运与 TCP Accept 循环。
// 仅供 route、proxy、frontend 等包共享；自身仅依赖 log。
package netutil

import (
	"errors"
	"io"
	"net"
	"net/http/httputil"
	"sync"
	"time"

	"mrp/internal/log"
)

const (
	copyBufferSize = 32 << 10 // 与 httputil.ReverseProxy 默认拷贝缓冲一致，供复用池按该尺寸分配
	UDPBufferSize  = 64 << 10 // 大于最大 UDP 载荷 65507 字节，保证单报文不截断，SOCKS5 UDP 中继共用

	acceptBackoffInitial = 5 * time.Millisecond
	acceptBackoffMax     = time.Second
)

var BufferPool httputil.BufferPool = &bufferPool{} // 复用 32KB 拷贝缓冲，relay 与 ReverseProxy 共用：sync.Pool 并发安全，避免每条流式响应或每条隧道各分配一次

// bufferPool 用 *[]byte 承载切片头，避免 sync.Pool 按 any 装箱切片时不必要的小分配；底层的 32KB 数组仍整体复用。
type bufferPool struct {
	pool sync.Pool
}

func (p *bufferPool) Get() []byte {
	if ptr, ok := p.pool.Get().(*[]byte); ok {
		return *ptr
	}
	return make([]byte, copyBufferSize)
}

func (p *bufferPool) Put(buf []byte) { p.pool.Put(&buf) }

// Relay 在 client 与 upstream 之间双向搬运字节：client→upstream 起协程，主协程跑 upstream→client；拷贝缓冲复用 BufferPool，任一方向结束后返回，由调用方关闭 client 打断对端读取使协程退出。
func Relay(client, upstream net.Conn) {
	upstreamBuf := BufferPool.Get()
	clientBuf := BufferPool.Get()
	go func() {
		defer BufferPool.Put(upstreamBuf)
		_, _ = io.CopyBuffer(upstream, client, upstreamBuf)
		upstream.Close()
	}()
	defer BufferPool.Put(clientBuf)
	_, _ = io.CopyBuffer(client, upstream, clientBuf)
}

// ServeListener 在 listener 上常驻 Accept 循环：每条连接启动 goroutine 调 handle，连接计数经 connections 跟踪供优雅退出；监听器已关闭即返回，其余 Accept 错误按退避重试。
func ServeListener(listener net.Listener, handle func(net.Conn), connections *sync.WaitGroup) error {
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
			handle(conn)
		}()
	}
}
