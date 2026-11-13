package proxy

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"mrp/internal/log"
	"mrp/internal/netutil"
)

// socks5UDPIdleTimeout 是 SOCKS5 UDP 关联的空闲超时：超过该时间无 UDP 报文的中继套接字自动关闭。
const socks5UDPIdleTimeout = 30 * time.Second

// socks5UDPHeader 解析后的 SOCKS5 UDP 报文头：host/port 为解码后的目标地址，data 为净荷，atyp/addr 为原始地址表示（供响应头原样回写），frag 非零表示分片（不支持重组）。
type socks5UDPHeader struct {
	host string
	port int
	data []byte
	atyp byte
	addr []byte
	frag byte
}

// serveSocks5UDP 处理 UDP ASSOCIATE 命令：绑定本地 127.0.0.1 的临时 UDP 中继套接字并回复其地址，随后阻塞至 TCP 控制连接关闭或中继空闲超时；TCP 关闭后释放中继及其全部会话。
func (p *Proxy) serveSocks5UDP(client net.Conn) {
	relayConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		log.Error("socks5 udp relay bind failed err=%v", err)
		_ = socks5WriteReply(client, socks5RepGeneralFailure)
		return
	}
	defer relayConn.Close()
	relayAddr := relayConn.LocalAddr().(*net.UDPAddr)
	if err := socks5WriteReplyAddr(client, socks5RepSuccess, net.IPv4(127, 0, 0, 1), relayAddr.Port); err != nil {
		return
	}
	log.Info("socks5 udp associated addr=%s", relayAddr)
	relay := newSocks5UDPRelay(relayConn, p.nameservers.Resolve)
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		relay.readLoop()
	}()
	tcpDone := make(chan struct{})
	go func() {
		defer close(tcpDone)
		_ = client.SetReadDeadline(time.Time{})
		_, _ = client.Read(make([]byte, 1)) // 阻塞至 TCP 控制连接关闭
	}()
	select {
	case <-tcpDone:
		relay.close()
		<-relayDone
	case <-relayDone:
		client.Close()
		<-tcpDone
	}
}

// socks5WriteReplyAddr 回写 SOCKS5 响应，BND.ADDR 为指定的 IPv4 地址与端口；常规回复用 0.0.0.0:0，UDP ASSOCIATE 回复用中继套接字实际地址。
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

// socks5UDPRelay 管理一个 SOCKS5 UDP 中继：监听套接字接收客户端报文转发到目标，每个目标一个会话（已连接 UDP 套接字）回传响应；resolve 经自定义 DNS 解析目标地址。
type socks5UDPRelay struct {
	conn       *net.UDPConn
	clientAddr atomic.Pointer[net.UDPAddr]
	mu         sync.Mutex
	sessions   map[string]*socks5UDPSession
	resolve    func(context.Context, string, int) ([]string, error)
}

// socks5UDPSession 一个客户端→目标方向的 UDP 会话：target 为已连接目标的对端套接字，atyp/addr/port 为客户端请求中的原始目标表示，供响应头回写；key 为目标地址键，供 O(1) 移除。
type socks5UDPSession struct {
	target *net.UDPConn
	relay  *socks5UDPRelay
	key    string
	atyp   byte
	addr   []byte
	port   int
}

func newSocks5UDPRelay(conn *net.UDPConn, resolve func(context.Context, string, int) ([]string, error)) *socks5UDPRelay {
	return &socks5UDPRelay{conn: conn, sessions: make(map[string]*socks5UDPSession), resolve: resolve}
}

// readLoop 读取客户端报文并转发：首个报文记录客户端源地址，分片或格式错误报文丢弃，读超时（空闲）或套接字关闭时返回。
func (r *socks5UDPRelay) readLoop() {
	buffer := make([]byte, netutil.UDPBufferSize)
	for {
		_ = r.conn.SetReadDeadline(time.Now().Add(socks5UDPIdleTimeout))
		n, client, err := r.conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		if r.clientAddr.Load() == nil {
			r.clientAddr.Store(client)
		} else if client.String() != r.clientAddr.Load().String() {
			continue // 只接受已记录客户端的报文
		}
		r.forwardPacket(buffer[:n])
	}
}

// forwardPacket 解析 SOCKS5 UDP 报文头并按目标转发净荷，无该目标的会话时创建并启动读回协程。
func (r *socks5UDPRelay) forwardPacket(packet []byte) {
	header, ok := parseSocks5UDPHeader(packet)
	if !ok || header.frag != 0 {
		return
	}
	key := net.JoinHostPort(header.host, strconv.Itoa(header.port))
	r.mu.Lock()
	session := r.sessions[key]
	if session == nil {
		targetConn, err := r.dialUDPTarget(header.host, header.port)
		if err != nil {
			r.mu.Unlock()
			log.Error("socks5 udp dial failed target=%s err=%v", key, err)
			return
		}
		session = &socks5UDPSession{
			target: targetConn,
			relay:  r,
			key:    key,
			atyp:   header.atyp,
			addr:   append([]byte(nil), header.addr...),
			port:   header.port,
		}
		r.sessions[key] = session
		go session.readLoop()
	}
	r.mu.Unlock()
	_, _ = session.target.Write(header.data)
}

// dialUDPTarget 解析目标并经自定义 DNS 拨号：resolve 为 nil 时回退系统 DNS；解析出多个地址时逐个尝试，返回第一个成功连接。
func (r *socks5UDPRelay) dialUDPTarget(host string, port int) (*net.UDPConn, error) {
	if r.resolve == nil {
		return dialUDP(net.JoinHostPort(host, strconv.Itoa(port)))
	}
	targets, err := r.resolve(context.Background(), host, port)
	if err != nil {
		return nil, err
	}
	var lastError error
	for _, target := range targets {
		conn, err := dialUDP(target)
		if err == nil {
			return conn, nil
		}
		lastError = err
	}
	return nil, lastError
}

// dialUDP 按 host:port 拨出已连接的 UDP 套接字，host 为 IP 字面量时不经 DNS。
func dialUDP(address string) (*net.UDPConn, error) {
	addr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	return net.DialUDP("udp", nil, addr)
}

// readLoop 读取目标返回的报文，补 SOCKS5 UDP 响应头后回传客户端；读超时或对端关闭时移除会话。
func (s *socks5UDPSession) readLoop() {
	buffer := make([]byte, netutil.UDPBufferSize)
	for {
		_ = s.target.SetReadDeadline(time.Now().Add(socks5UDPIdleTimeout))
		n, err := s.target.Read(buffer)
		if err != nil {
			break
		}
		client := s.relay.clientAddr.Load()
		if client == nil {
			continue
		}
		response := encodeSocks5UDPResponse(s.atyp, s.addr, s.port, buffer[:n])
		_, _ = s.relay.conn.WriteToUDP(response, client)
	}
	s.relay.removeSession(s)
}

// removeSession 关闭并移除指定会话，按会话 key 直接定位。
func (r *socks5UDPRelay) removeSession(session *socks5UDPSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.sessions[session.key]; ok && existing == session {
		_ = session.target.Close()
		delete(r.sessions, session.key)
	}
}

// close 关闭中继套接字并清理全部会话，使读循环退出。
func (r *socks5UDPRelay) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.conn.Close()
	for key, session := range r.sessions {
		_ = session.target.Close()
		delete(r.sessions, key)
	}
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

// encodeSocks5UDPResponse 按请求中的目标地址原样回写响应头并拼接净荷；domain 类型在地址前补长度字节。
func encodeSocks5UDPResponse(atyp byte, addr []byte, port int, data []byte) []byte {
	addrOffset := 0
	if atyp == socks5AtypDomain {
		addrOffset = 1
	}
	header := make([]byte, 4+addrOffset+len(addr)+2+len(data))
	header[3] = atyp
	addrStart := 4
	if atyp == socks5AtypDomain {
		header[addrStart] = byte(len(addr))
		addrStart++
	}
	copy(header[addrStart:], addr)
	binary.BigEndian.PutUint16(header[addrStart+len(addr):], uint16(port))
	copy(header[addrStart+len(addr)+2:], data)
	return header
}
