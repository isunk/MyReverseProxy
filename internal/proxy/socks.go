package proxy

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"time"

	"mrp/internal/log"
	"mrp/internal/netutil"
)

// SOCKS5 常量：仅实现无认证的 CONNECT 与 UDP ASSOCIATE，BIND 返回命令不支持。
const (
	socks5Version               = 0x05
	socks5MethodNoAuth          = 0x00
	socks5MethodNoAcceptable    = 0xFF
	socks5CommandConnect        = 0x01
	socks5CommandUDPAssociate   = 0x03
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

// serveSocks5 处理一条 SOCKS5 连接：完成无认证握手后按命令分发 CONNECT 透传或 UDP ASSOCIATE 中继。
func (p *Proxy) serveSocks5(client net.Conn) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(socks5HandshakeTimeout))
	reader := bufio.NewReader(client)
	if !socks5Greeting(reader, client) {
		return
	}
	command, atyp, rep := socks5ReadCommand(reader)
	if rep != socks5RepSuccess {
		_ = socks5WriteReply(client, rep)
		return
	}
	switch command {
	case socks5CommandConnect:
		host, port, addrRep := socks5ReadAddr(reader, atyp)
		if addrRep != socks5RepSuccess {
			_ = socks5WriteReply(client, addrRep)
			return
		}
		p.serveSocks5Connect(client, host, port)
	case socks5CommandUDPAssociate:
		_, _, _ = socks5ReadAddr(reader, atyp) // UDP ASSOCIATE 请求中的 DST.ADDR/PORT 是客户端预期来源，通常为 0.0.0.0:0，仅消费不校验
		p.serveSocks5UDP(client)
	}
}

// serveSocks5Connect 处理 CONNECT 命令：拨号目标并透传字节，不查路由表、不做 MITM。
func (p *Proxy) serveSocks5Connect(client net.Conn, host string, port int) {
	_ = client.SetDeadline(time.Time{})
	upstream, err := p.dialTunnel(host, port)
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
	netutil.Relay(client, upstream)
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

// socks5ReadCommand 读取请求头并返回命令与地址类型；支持 CONNECT 与 UDP ASSOCIATE，其余命令返回不支持。
func socks5ReadCommand(reader *bufio.Reader) (command, atyp, rep byte) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, 0, socks5RepGeneralFailure
	}
	if header[0] != socks5Version {
		return 0, 0, socks5RepGeneralFailure
	}
	switch header[1] {
	case socks5CommandConnect, socks5CommandUDPAssociate:
		return header[1], header[3], socks5RepSuccess
	default:
		return 0, 0, socks5RepCommandUnsupported
	}
}

// socks5ReadAddr 从 reader 读取 ATYP 对应的地址与端口，返回失败时 rep 非 success 供写回。
func socks5ReadAddr(reader *bufio.Reader, atyp byte) (host string, port int, rep byte) {
	var raw []byte
	switch atyp {
	case socks5AtypIPv4:
		raw = make([]byte, net.IPv4len)
	case socks5AtypIPv6:
		raw = make([]byte, net.IPv6len)
	case socks5AtypDomain:
		length, err := reader.ReadByte()
		if err != nil || length == 0 {
			return "", 0, socks5RepGeneralFailure
		}
		raw = make([]byte, int(length))
	default:
		return "", 0, socks5RepAtypUnsupported
	}
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", 0, socks5RepGeneralFailure
	}
	host, ok := socks5DecodeHost(atyp, raw)
	if !ok {
		return "", 0, socks5RepGeneralFailure
	}
	portRaw := make([]byte, 2)
	if _, err := io.ReadFull(reader, portRaw); err != nil {
		return "", 0, socks5RepGeneralFailure
	}
	return host, int(binary.BigEndian.Uint16(portRaw)), socks5RepSuccess
}

// socks5DecodeHost 按 ATYP 从地址字节解码主机字符串，raw 须恰为该 ATYP 的地址长度。
func socks5DecodeHost(atyp byte, raw []byte) (string, bool) {
	switch atyp {
	case socks5AtypIPv4:
		if len(raw) != net.IPv4len {
			return "", false
		}
		return net.IP(raw).String(), true
	case socks5AtypIPv6:
		if len(raw) != net.IPv6len {
			return "", false
		}
		return net.IP(raw).String(), true
	case socks5AtypDomain:
		return string(raw), len(raw) > 0
	default:
		return "", false
	}
}

// socks5WriteReply 回写 SOCKS5 响应，BND.ADDR 固定填 IPv4 0.0.0.0:0。
func socks5WriteReply(writer io.Writer, rep byte) error {
	return socks5WriteReplyAddr(writer, rep, net.IPv4zero, 0) // 0.0.0.0:0 为占位值，RFC 允许省略，客户端不依赖该值
}
