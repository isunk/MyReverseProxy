package proxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"time"

	"mrp/internal/log"
)

// SOCKS5 常量：仅实现无认证的 CONNECT，BIND 与 UDP ASSOCIATE 返回命令不支持。
const (
	socks5Version               = 0x05
	socks5MethodNoAuth          = 0x00
	socks5MethodNoAcceptable    = 0xFF
	socks5CommandConnect        = 0x01
	socks5AtypIPv4              = 0x01
	socks5AtypDomain            = 0x03
	socks5AtypIPv6              = 0x04
	socks5RepSuccess            = 0x00
	socks5RepGeneralFailure     = 0x01
	socks5RepHostUnreachable    = 0x04
	socks5RepCommandUnsupported = 0x07
	socks5RepAtypUnsupported    = 0x08

	socks5HandshakeTimeout = 10 * time.Second
)

// serveSocks5 处理一条 SOCKS5 连接：完成无认证握手后按 CONNECT 目标拨号并透传字节，不查路由表、不做 MITM。
func (p *Proxy) serveSocks5(client net.Conn) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(socks5HandshakeTimeout))
	reader := bufio.NewReader(client)
	if !socks5Greeting(reader, client) {
		return
	}
	host, port, rep := socks5ReadConnect(reader)
	if rep != socks5RepSuccess {
		_ = socks5WriteReply(client, rep)
		return
	}
	_ = client.SetDeadline(time.Time{})
	ctx, cancel := context.WithTimeout(context.Background(), tunnelDialTimeout)
	defer cancel()
	upstream, err := p.nameservers.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		log.Error("socks5 upstream dial failed host=%s port=%d err=%v", host, port, err)
		_ = socks5WriteReply(client, socks5RepHostUnreachable)
		return
	}
	defer upstream.Close()
	if err := socks5WriteReply(client, socks5RepSuccess); err != nil {
		return
	}
	log.Info("socks5 connect started host=%s port=%d", host, port)
	relay(client, upstream)
}

// socks5Greeting 完成版本协商：仅支持无认证，返回 false 表示版本不符或无可接受方法，连接应关闭。
func socks5Greeting(reader *bufio.Reader, writer io.Writer) bool {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return false
	}
	if header[0] != socks5Version {
		return false
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, methods); err != nil {
		return false
	}
	for _, method := range methods {
		if method == socks5MethodNoAuth {
			_, _ = writer.Write([]byte{socks5Version, socks5MethodNoAuth})
			return true
		}
	}
	_, _ = writer.Write([]byte{socks5Version, socks5MethodNoAcceptable})
	return false
}

// socks5ReadConnect 解析 CONNECT 请求的地址与端口，返回失败时 rep 非 success 供写回。
func socks5ReadConnect(reader *bufio.Reader) (host string, port int, rep byte) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return "", 0, socks5RepGeneralFailure
	}
	if header[0] != socks5Version {
		return "", 0, socks5RepGeneralFailure
	}
	if header[1] != socks5CommandConnect {
		return "", 0, socks5RepCommandUnsupported
	}
	var address string
	switch header[3] {
	case socks5AtypIPv4:
		raw := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(reader, raw); err != nil {
			return "", 0, socks5RepGeneralFailure
		}
		address = net.IP(raw).String()
	case socks5AtypIPv6:
		raw := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(reader, raw); err != nil {
			return "", 0, socks5RepGeneralFailure
		}
		address = net.IP(raw).String()
	case socks5AtypDomain:
		length, err := reader.ReadByte()
		if err != nil || length == 0 {
			return "", 0, socks5RepGeneralFailure
		}
		raw := make([]byte, int(length))
		if _, err := io.ReadFull(reader, raw); err != nil {
			return "", 0, socks5RepGeneralFailure
		}
		address = string(raw)
	default:
		return "", 0, socks5RepAtypUnsupported
	}
	portRaw := make([]byte, 2)
	if _, err := io.ReadFull(reader, portRaw); err != nil {
		return "", 0, socks5RepGeneralFailure
	}
	return address, int(binary.BigEndian.Uint16(portRaw)), socks5RepSuccess
}

// socks5WriteReply 回写 SOCKS5 响应，绑定地址固定填 IPv4 0.0.0.0:0（RFC 允许省略，客户端不依赖该值）。
func socks5WriteReply(writer io.Writer, rep byte) error {
	_, err := writer.Write([]byte{socks5Version, rep, 0x00, socks5AtypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}
