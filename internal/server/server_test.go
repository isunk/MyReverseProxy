package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"mrp/internal/log"
)

func init() {
	log.SetOutput(io.Discard)
}

// scriptedListener 按脚本依次返回错误与连接，模拟 Accept 的瞬时故障与恢复。
type scriptedListener struct {
	accept    func() (net.Conn, error)
	closed    chan struct{}
	closeOnce sync.Once
}

func (l *scriptedListener) Accept() (net.Conn, error) { return l.accept() }

func (l *scriptedListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *scriptedListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
}

// Accept 先失败两次再恢复供连，验证瞬时错误后退避重试且连接被正常服务；
// 监听器关闭后 Serve 以 net.ErrClosed 返回。
func TestServe_RetriesTransientAcceptErrors(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	state := 0
	listener := &scriptedListener{closed: make(chan struct{})}
	listener.accept = func() (net.Conn, error) {
		switch state {
		case 0, 1:
			state++
			return nil, errors.New("accept: too many open files")
		case 2:
			state++
			return serverConn, nil
		default:
			<-listener.closed
			return nil, net.ErrClosed
		}
	}
	handlerCalled := make(chan struct{})
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		close(handlerCalled)
	})

	serveDone := make(chan error, 1)
	go func() { serveDone <- Serve(listener, handler) }()

	clientConn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(clientConn, "GET / HTTP/1.1\r\nHost: t\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(clientConn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read response after transient accept errors: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	select {
	case <-handlerCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("handler was not invoked")
	}
	clientConn.Close()
	listener.Close()

	select {
	case err := <-serveDone:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Serve err = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after listener close")
	}
}

func TestServe_ReturnsOnListenerClose(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- Serve(listener, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	}()
	listener.Close()
	select {
	case err := <-serveDone:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Serve err = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after listener close")
	}
}
