package netutil

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"mrp/internal/log"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	m.Run()
}

func TestRelay(t *testing.T) {
	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer echoListener.Close()
	go func() {
		for {
			conn, err := echoListener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) { io.Copy(conn, conn); conn.Close() }(conn)
		}
	}()

	upstream, err := net.Dial("tcp", echoListener.Addr().String())
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	defer upstream.Close()
	clientA, clientB := net.Pipe()
	defer clientA.Close()
	defer clientB.Close()

	relayDone := make(chan struct{})
	go func() {
		Relay(clientA, upstream)
		close(relayDone)
	}()

	payload := "hello relay"
	if _, err := clientB.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buffer := make([]byte, len(payload))
	_ = clientB.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(clientB, buffer); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buffer) != payload {
		t.Fatalf("echo = %q, want %q", buffer, payload)
	}
	clientA.Close()
	<-relayDone
}

func TestServeListenerClosesOnListenerClose(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	connections := &sync.WaitGroup{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- ServeListener(listener, func(net.Conn) {}, connections)
	}()
	listener.Close()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("ServeListener returned nil, want closed error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeListener did not return after listener close")
	}
}

func TestServeListenerHandlesConnections(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	connections := &sync.WaitGroup{}
	received := make(chan struct{}, 1)
	go func() {
		_ = ServeListener(listener, func(conn net.Conn) {
			conn.Close()
			received <- struct{}{}
		}, connections)
	}()
	defer listener.Close()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()
	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("handler not called")
	}
}

func TestBufferPoolReuse(t *testing.T) {
	first := BufferPool.Get()
	BufferPool.Put(first)
	second := BufferPool.Get()
	if len(first) != len(second) {
		t.Fatalf("buffer sizes differ: %d vs %d", len(first), len(second))
	}
	BufferPool.Put(second)
}
