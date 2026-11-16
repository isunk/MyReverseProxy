package proxy

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"mrp/internal/log"
	"mrp/internal/netutil"
)

// serveSocks5UDP 处理 UDP ASSOCIATE：绑定本地临时 UDP 中继并回复地址，阻塞至控制连接关闭或空闲超时。
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
	relay := newSocks5UDPRelay(relayConn, p.nameservers.Resolve, p.udpIdleTimeout)
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		relay.readLoop()
	}()
	tcpDone := make(chan struct{})
	go func() {
		defer close(tcpDone)
		_ = client.SetReadDeadline(time.Time{})
		buffer := make([]byte, 1)
		for {
			if _, err := client.Read(buffer); err != nil {
				return // 读到 EOF/错误才算控制连接关闭；收到数据（如 keepalive）不拆除中继
			}
		}
	}()
	select {
	case <-tcpDone:
		relay.close()
		<-relayDone
	case <-relayDone:
		// 中继先结束（空闲超时或出错）同样清理全部会话套接字与读回协程，避免泄漏。
		relay.close()
		client.Close()
		<-tcpDone
	}
}

// socks5UDPRelay 管理 SOCKS5 UDP 中继：客户端报文按目标分发到会话回传响应，resolve 解析目标地址。
type socks5UDPRelay struct {
	conn        *net.UDPConn
	clientAddr  atomic.Pointer[net.UDPAddr]
	mu          sync.Mutex
	sessions    map[string]*socks5UDPSession
	resolve     func(context.Context, string, int) ([]string, error)
	closed      atomic.Bool
	idleTimeout time.Duration
}

// socks5UDPSession 客户端→目标方向的 UDP 会话：target 为对端套接字，buffer 复用缓冲，key 为目标地址键。
type socks5UDPSession struct {
	target       *net.UDPConn
	relay        *socks5UDPRelay
	key          string
	headerPrefix []byte
	buffer       []byte
}

func newSocks5UDPRelay(conn *net.UDPConn, resolve func(context.Context, string, int) ([]string, error), idleTimeout time.Duration) *socks5UDPRelay {
	return &socks5UDPRelay{
		conn:        conn,
		sessions:    make(map[string]*socks5UDPSession),
		resolve:     resolve,
		idleTimeout: idleTimeout,
	}
}

// readLoop 读取客户端报文并转发：首个报文记录源地址，分片或格式错误丢弃，读超时或套接字关闭返回。
func (r *socks5UDPRelay) readLoop() {
	buffer := make([]byte, netutil.UDPBufferSize)
	for {
		_ = r.conn.SetReadDeadline(time.Now().Add(r.idleTimeout))
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

// forwardPacket 解析报文头按目标转发，无会话时创建并启动读回协程；仅 readLoop 单 goroutine 调用，同 key 不并发。
func (r *socks5UDPRelay) forwardPacket(packet []byte) {
	header, ok := parseSocks5UDPHeader(packet)
	if !ok || header.frag != 0 {
		return
	}
	key := net.JoinHostPort(header.host, strconv.Itoa(header.port))
	r.mu.Lock()
	session := r.sessions[key]
	r.mu.Unlock()
	if session == nil {
		targetConn, err := r.dialUDPTarget(header.host, header.port)
		if err != nil {
			log.Error("socks5 udp dial failed target=%s err=%v", key, err)
			return
		}
		r.mu.Lock()
		if r.closed.Load() { // 拨号期间中继已被关闭（TCP 控制连接结束）：丢弃新拨出的套接字
			r.mu.Unlock()
			_ = targetConn.Close()
			return
		}
		session = &socks5UDPSession{
			target:       targetConn,
			relay:        r,
			key:          key,
			headerPrefix: socks5ResponsePrefix(header.atyp, header.addr, header.port),
		}
		r.sessions[key] = session
		r.mu.Unlock()
		go session.readLoop()
	}
	_, _ = session.target.Write(header.data)
}

// dialUDPTarget 解析目标并经 DNS 拨号：resolve 为 nil 回退系统 DNS，多地址逐个尝试，无可用地址返回错误。
func (r *socks5UDPRelay) dialUDPTarget(host string, port int) (*net.UDPConn, error) {
	address := net.JoinHostPort(host, strconv.Itoa(port))
	if r.resolve == nil {
		return dialUDP(address)
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
	if lastError == nil {
		lastError = fmt.Errorf("no addresses resolved for %s", address)
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
		_ = s.target.SetReadDeadline(time.Now().Add(s.relay.idleTimeout))
		n, err := s.target.Read(buffer)
		if err != nil {
			break
		}
		client := s.relay.clientAddr.Load()
		if client == nil {
			continue
		}
		_, _ = s.relay.conn.WriteToUDP(s.wrap(buffer[:n]), client)
	}
	s.relay.removeSession(s)
}

// wrap 用会话固定的响应头前缀与复用缓冲打包净荷，避免每报文一次堆分配；buffer 仅在净荷超出容量时增长。
func (s *socks5UDPSession) wrap(data []byte) []byte {
	total := len(s.headerPrefix) + len(data)
	if cap(s.buffer) < total {
		s.buffer = make([]byte, total)
	}
	out := s.buffer[:total]
	copy(out, s.headerPrefix)
	copy(out[len(s.headerPrefix):], data)
	return out
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

// close 关闭中继套接字并清理全部会话，使读循环退出；置位 closed 供拨号中的前向转发丢弃新套接字。
func (r *socks5UDPRelay) close() {
	r.closed.Store(true)
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.conn.Close()
	for key, session := range r.sessions {
		_ = session.target.Close()
		delete(r.sessions, key)
	}
}
