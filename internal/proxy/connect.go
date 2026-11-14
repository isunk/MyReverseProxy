package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"mrp/internal/ca"
	"mrp/internal/frontend"
	"mrp/internal/log"
	"mrp/internal/netutil"
)

const (
	connectEstablished   = "HTTP/1.1 200 Connection Established\r\n\r\n"
	connectBadGateway    = "HTTP/1.1 502 Bad Gateway\r\n\r\n"
	tunnelDialTimeout    = 10 * time.Second
	mitmHandshakeTimeout = 10 * time.Second
)

func (p *Proxy) handleConnect(writer http.ResponseWriter, request *http.Request) {
	protocol, port, domain := targetOf(request)
	client, ok := hijackConn(writer)
	if !ok {
		return
	}
	// serveConnect 未接管（transferred=false）的连接仍在本函数手里，由这里关闭收尾
	if transferred := p.serveConnect(client, protocol, port, domain); !transferred {
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
	return &frontend.BufferedConn{Conn: client, Reader: bufioRW.Reader}, true // 保留 http.Server 内部 bufio.Reader 预读的数据（客户端紧跟 CONNECT 发送的字节）
}

// serveConnect 按 CONNECT 目标分发：命中虚拟服务器走 MITM，否则透传隧道；transferred 为 true 表示连接已移交内层服务，调用方不得再关闭。
func (p *Proxy) serveConnect(client net.Conn, protocol string, port int, domain string) (transferred bool) {
	if !p.table.Load().Has(protocol, port, domain) {
		log.Info("tunnel started domain=%s port=%d", domain, port)
		p.tunnel(client, domain, port)
		return false // 隧道按规范化目标拨号：Host 缺端口补协议默认端口，IPv6 裸地址补括号，否则 SplitHostPort 报错使目标不可达
	}
	if p.tlsConfig == nil {
		log.Warn("mitm skipped domain=%s port=%d: no ca certificate", domain, port)
		_, _ = client.Write([]byte(connectBadGateway))
		return false
	}
	log.Info("mitm started domain=%s port=%d", domain, port)
	if _, err := client.Write([]byte(connectEstablished)); err != nil {
		return false
	}
	tlsConn := tls.Server(client, mitmTLSConfig(p.tlsConfig, p.authority, domain))
	if err := tlsHandshake(tlsConn); err != nil {
		logMitmHandshakeError(domain, port, err)
		tlsConn.Close()
		return false
	}
	return frontend.ServeSingleConn(frontend.New(p.Handler()), tlsConn)
}

// mitmTLSConfig 为单条 MITM 隧道构造 TLS 配置：SNI 缺失时回退到 CONNECT 目标域名再签发；静态证书或无 CA 的构造原样返回。
func mitmTLSConfig(template *tls.Config, authority *ca.Authority, domain string) *tls.Config {
	if template.GetCertificate == nil || authority == nil {
		return template
	}
	connConfig := template.Clone()
	connConfig.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		// 客户端对 IP 直连不发 SNI，握手回调拿不到目标，故回退到 CONNECT 目标域名再签发
		if hello.ServerName == "" {
			hello.ServerName = domain
		}
		return authority.GetCertificate(hello)
	}
	return connConfig
}

// untrustedCAAlerts 列出客户端因校验证书失败（典型为未安装/信任代理 CA）而回送的 TLS alert 文本，与 crypto/tls alert 字符串对齐。
var untrustedCAAlerts = map[string]bool{
	"tls: bad certificate":               true,
	"tls: unsupported certificate":       true,
	"tls: unknown certificate":           true,
	"tls: unknown certificate authority": true,
}

// tlsHandshake 在握手超时内完成 MITM TLS 握手，超时后清除 deadline 交还连接给 http.Server 继续读写应用数据。
func tlsHandshake(conn *tls.Conn) error {
	_ = conn.SetDeadline(time.Now().Add(mitmHandshakeTimeout))
	defer conn.SetDeadline(time.Time{})
	return conn.Handshake()
}

// logMitmHandshakeError 识别握手失败原因：客户端未信任代理 CA 证书时给出可操作提示，其余仍原样记录。
func logMitmHandshakeError(domain string, port int, err error) {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "remote error" && untrustedCAAlerts[opErr.Err.Error()] {
		log.Warn("mitm handshake failed domain=%s port=%d reason=client does not trust proxy ca certificate hint=install and trust the ca certificate err=%v", domain, port, err)
		return
	}
	log.Warn("mitm handshake failed domain=%s port=%d err=%v", domain, port, err)
}

// dialTunnel 以隧道超时经 nameservers 拨号目标。
func (p *Proxy) dialTunnel(host string, port int) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tunnelDialTimeout)
	defer cancel()
	return p.nameservers.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
}

// tunnel 在客户端与目标之间双向搬运字节，不做任何内容检查或改写。
func (p *Proxy) tunnel(client net.Conn, domain string, port int) {
	upstream, err := p.dialTunnel(domain, port)
	if err != nil {
		log.Error("upstream dial failed domain=%s port=%d err=%v", domain, port, err)
		_, _ = client.Write([]byte(connectBadGateway))
		return
	}
	defer upstream.Close()
	if _, err := client.Write([]byte(connectEstablished)); err != nil {
		return
	}
	netutil.Relay(client, upstream)
}
