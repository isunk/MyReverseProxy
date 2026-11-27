// Package inspect 以 HTTP 提供实时 HAR 文档，供桌面 Chrome DevTools 网络面板原生导入分析代理可见的请求/响应。
package inspect

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"time"

	"mrp/internal/capture"
)

const readHeaderTimeout = 10 * time.Second

// Server 承载实时 HAR 的 HTTP 服务：GET / 返回当前保留交互打包成的 HAR 文件。
type Server struct {
	session  *capture.Session
	addr     string
	listener net.Listener
	server   *http.Server
}

// New 构建服务实例，addr 为监听地址（host:port），由 Start 绑定。
func New(session *capture.Session, addr string) *Server {
	instance := &Server{session: session, addr: addr}
	mux := http.NewServeMux()
	mux.HandleFunc("/", instance.serveHar)
	instance.server = &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout}
	return instance
}

// Addr 返回监听地址，供调用方在启动失败日志中定位。
func (s *Server) Addr() string { return s.addr }

// Start 绑定监听地址并异步服务，返回绑定错误（同步，供启动阶段快速失败）。
func (s *Server) Start() error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.listener = listener
	go func() { _ = s.server.Serve(listener) }()
	return nil
}

// Close 关闭监听，令后台 Serve 退出。
func (s *Server) Close() error {
	if s.listener == nil {
		return nil
	}
	return s.listener.Close()
}

// serveHar 返回当前实时 HAR：以附件形式下载，导入 Chrome DevTools 网络面板。
func (s *Server) serveHar(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	data, err := json.Marshal(s.session.HAR())
	if err != nil {
		http.Error(writer, "marshal HAR", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Content-Disposition", `attachment; filename="mrp.har"`)
	writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
	writer.WriteHeader(http.StatusOK)
	if request.Method != http.MethodHead {
		_, _ = writer.Write(data)
	}
}
