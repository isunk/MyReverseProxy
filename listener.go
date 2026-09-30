package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
)

// 监听协议常量：server.protocol 的合法取值；留空表示自适应（按连接首字节识别）
const (
	protocolHTTP  = "http"
	protocolHTTPS = "https"
)

// listenerSpec 一个监听端口的规格：端口与协议，协议留空表示自适应
type listenerSpec struct {
	port     int
	protocol string
}

// listenerEntry 一条运行中的监听
type listenerEntry struct {
	spec listenerSpec
	ln   net.Listener
}

// listenerSet 端口监听集：按配置规格增量建立与回收监听。
// 关闭监听仅停止 Accept，已接受连接由各自 goroutine 自然结束。
type listenerSet struct {
	mu      sync.Mutex
	entries map[int]*listenerEntry
}

func newListenerSet() *listenerSet {
	return &listenerSet{entries: map[int]*listenerEntry{}}
}

// reconcile 按目标规格增量对账：规格变化的端口先关旧，新增端口逐条建立，
// 任一建立失败即回滚本次新增并保留既有监听；规格未变的端口保持原监听不中断。
func (s *listenerSet) reconcile(specs []listenerSpec, tlsConfig *tls.Config, handler func(spec listenerSpec) http.Handler) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	desired := make(map[int]listenerSpec, len(specs))
	for _, spec := range specs {
		desired[spec.port] = spec
	}
	for port, entry := range s.entries {
		if desired[port] == entry.spec {
			continue
		}
		entry.ln.Close()
		delete(s.entries, port)
		logInfof("listener closed port=%d", port)
	}

	added := map[int]*listenerEntry{}
	for _, spec := range specs {
		if _, ok := s.entries[spec.port]; ok {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", spec.port))
		if err != nil {
			for _, entry := range added {
				entry.ln.Close()
				delete(s.entries, entry.spec.port)
			}
			return fmt.Errorf("listen port %d: %w", spec.port, err)
		}
		entry := &listenerEntry{spec: spec, ln: ln}
		s.entries[spec.port] = entry
		added[spec.port] = entry
		go serve(ln, tlsConfig, handler(spec), spec.protocol)
		logInfof("listening addr=:%d protocol=%s", spec.port, protocolLabel(spec.protocol))
	}

	if len(specs) == 0 {
		logWarnf("config has no servers, no ports to listen")
	}
	return nil
}

// protocolLabel 日志展示用协议名，留空显示为 auto
func protocolLabel(protocol string) string {
	if protocol == "" {
		return "auto"
	}
	return protocol
}

// closeAll 关闭全部监听，供测试与关停路径回收
func (s *listenerSet) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for port, entry := range s.entries {
		entry.ln.Close()
		delete(s.entries, port)
	}
}
