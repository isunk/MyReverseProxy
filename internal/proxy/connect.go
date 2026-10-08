package proxy

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"time"

	"mrp/internal/ca"
	"mrp/internal/log"
	"mrp/internal/server"
)

const (
	connectEstablished = "HTTP/1.1 200 Connection Established\r\n\r\n"
	connectBadGateway  = "HTTP/1.1 502 Bad Gateway\r\n\r\n"
	tunnelDialTimeout  = 10 * time.Second
)

func (p *Proxy) handleConnect(writer http.ResponseWriter, request *http.Request) {
	protocol, port, domain := targetOf(request)
	client, ok := hijackConn(writer)
	if !ok {
		return
	}
	// serveConnect 返回 false 表示连接仍在手里（隧道结束、502 或握手失败），由这里收尾
	if !p.serveConnect(client, request, protocol, port, domain) {
		client.Close()
	}
}

func hijackConn(writer http.ResponseWriter) (net.Conn, bool) {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		http.Error(writer, "hijack unsupported", http.StatusInternalServerError)
		return nil, false
	}
	client, bufioRW, err := hijacker.Hijack()
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	// 保留 http.Server 内部 bufio.Reader 预读的数据（客户端紧跟 CONNECT 发送的字节）
	return &server.BufferedConn{Conn: client, Reader: bufioRW.Reader}, true
}

// serveConnect 按 CONNECT 目标分发：命中虚拟服务器走 MITM，否则透传隧道；
// 返回 true 表示连接已移交内层 HTTP 服务（hijack 链），调用方不得再关闭
func (p *Proxy) serveConnect(client net.Conn, request *http.Request, protocol string, port int, domain string) bool {
	if !p.table.Load().Has(protocol, port, domain) {
		log.Info("connect target=%s mode=tunnel", request.Host)
		p.tunnel(client, request.Host)
		return false
	}
	if p.tlsConfig == nil {
		log.Warn("connect domain=%s matched but no ca certificate configured, cannot mitm", domain)
		_, _ = client.Write([]byte(connectBadGateway))
		return false
	}
	log.Info("connect domain=%s port=%d mode=mitm", domain, port)
	if _, err := client.Write([]byte(connectEstablished)); err != nil {
		return false
	}
	return server.ServeSingleConn(server.New(p.Handler()), tls.Server(client, mitmTLSConfig(p.tlsConfig, p.authority, domain)))
}

// mitmTLSConfig 为单条 MITM 隧道构造 TLS 配置：客户端对 IP 直连不发 SNI，
// 握手回调拿不到目标，故在按名字签发的配置上（GetCertificate 与 CA 均可用时）
// 将 SNI 缺失的请求回退到 CONNECT 目标签名，使 IP 型 HTTPS 路由同样能 MITM。
// 静态证书或无 CA 的构造原样返回，SNI 缺失时维持原行为。
func mitmTLSConfig(template *tls.Config, authority *ca.Authority, domain string) *tls.Config {
	if template.GetCertificate == nil || authority == nil {
		return template
	}
	connConfig := template.Clone()
	connConfig.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello.ServerName == "" {
			hello.ServerName = domain
		}
		return authority.GetCertificate(hello)
	}
	return connConfig
}

// tunnel 在客户端与目标之间双向搬运字节，不做任何内容检查或改写。
func (p *Proxy) tunnel(client net.Conn, target string) {
	ctx, cancel := context.WithTimeout(context.Background(), tunnelDialTimeout)
	defer cancel()
	upstream, err := p.nameservers.DialContext(ctx, "tcp", target)
	if err != nil {
		log.Error("tunnel target connection failed target=%s: %v", target, err)
		_, _ = client.Write([]byte(connectBadGateway))
		return
	}
	defer upstream.Close()
	if _, err := client.Write([]byte(connectEstablished)); err != nil {
		return
	}
	// 双向拷贝：client→upstream 起协程，主协程跑 upstream→client。任一方向结束后
	// tunnel 返回，由调用方关闭 client，进而打断对端读取使协程退出。
	go func() {
		io.Copy(upstream, client)
		upstream.Close()
	}()
	io.Copy(client, upstream)
}
