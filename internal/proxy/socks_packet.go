package proxy

import (
	"encoding/binary"
	"io"
	"net"
)

// socks5UDPHeader 解析后的 SOCKS5 UDP 报文头：host/port 为解码目标地址，data 为净荷，frag 非零表示分片。
type socks5UDPHeader struct {
	host string
	port int
	data []byte
	atyp byte
	addr []byte
	frag byte
}

// socks5WriteReply 回写 SOCKS5 响应，BND.ADDR 固定填 IPv4 0.0.0.0:0。
func socks5WriteReply(writer io.Writer, rep byte) error {
	// 0.0.0.0:0 为占位值，RFC 允许省略，客户端不依赖该值。
	return socks5WriteReplyAddr(writer, rep, net.IPv4zero, 0)
}

// socks5WriteReplyAddr 回写 SOCKS5 响应，BND.ADDR 为指定 IPv4 地址与端口。
func socks5WriteReplyAddr(writer io.Writer, rep byte, ip net.IP, port int) error {
	addr := ip.To4()
	header := []byte{
		socks5Version, rep, 0x00, socks5AtypIPv4,
		addr[0], addr[1], addr[2], addr[3],
		byte(port >> 8), byte(port),
	}
	_, err := writer.Write(header)
	return err
}

// parseSocks5UDPHeader 解析 SOCKS5 UDP 报文头（RSV 2B + FRAG 1B + ATYP + DST.ADDR + DST.PORT + DATA）。
func parseSocks5UDPHeader(packet []byte) (socks5UDPHeader, bool) {
	var header socks5UDPHeader
	if len(packet) < 4 || packet[0] != 0 || packet[1] != 0 {
		return header, false
	}
	header.frag = packet[2]
	atyp := packet[3]
	offset := 4
	switch atyp {
	case socks5AtypIPv4:
		if len(packet) < offset+net.IPv4len+2 {
			return header, false
		}
		header.atyp = atyp
		header.addr = packet[offset : offset+net.IPv4len]
		offset += net.IPv4len
	case socks5AtypIPv6:
		if len(packet) < offset+net.IPv6len+2 {
			return header, false
		}
		header.atyp = atyp
		header.addr = packet[offset : offset+net.IPv6len]
		offset += net.IPv6len
	case socks5AtypDomain:
		if len(packet) < offset+1 {
			return header, false
		}
		length := int(packet[offset])
		offset++
		if length == 0 || len(packet) < offset+length+2 {
			return header, false
		}
		header.atyp = atyp
		header.addr = packet[offset : offset+length]
		offset += length
	default:
		return header, false
	}
	host, ok := socks5DecodeHost(header.atyp, header.addr)
	if !ok {
		return header, false
	}
	header.host = host
	header.port = int(binary.BigEndian.Uint16(packet[offset : offset+2]))
	offset += 2
	header.data = packet[offset:]
	return header, true
}

// socks5ResponsePrefix 预计算固定响应头前缀（RSV+FRAG+ATYP+DST.ADDR+DST.PORT），供每报文复用。
func socks5ResponsePrefix(atyp byte, addr []byte, port int) []byte {
	addrOffset := 0
	if atyp == socks5AtypDomain {
		addrOffset = 1
	}
	prefix := make([]byte, 4+addrOffset+len(addr)+2)
	prefix[3] = atyp
	addrStart := 4
	if atyp == socks5AtypDomain {
		prefix[addrStart] = byte(len(addr))
		addrStart++
	}
	copy(prefix[addrStart:], addr)
	binary.BigEndian.PutUint16(prefix[addrStart+len(addr):], uint16(port))
	return prefix
}
