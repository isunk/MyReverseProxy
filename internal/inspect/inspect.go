// Package inspect 实现 Chrome DevTools 协议的最小端点，令 mrp 以网络目标出现在 chrome://inspect 中，
// DevTools 网络面板可原生实时查看代理可见的全部请求/响应。
package inspect

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"mrp/internal/capture"
)

const (
	readHeaderTimeout = 10 * time.Second
	// harTimeLayout HAR 1.2 时间戳格式，与 proxy 抓包保持一致。
	harTimeLayout = "2006-01-02T15:04:05.000Z07:00"
	// pollInterval hub 轮询会话新增记录的间隔，越小实时性越好，越大越省 CPU。
	pollInterval = 200 * time.Millisecond
	// sendBuffer conn 发送队列容量，超出后丢弃事件，避免慢客户端阻塞广播。
	sendBuffer = 2048

	// pageID 固定页面目标标识，mrp 以单一聚合目标呈现全部代理流量。
	pageID = "mrp"
	// targetURL 单一目标的占位页面地址，DevTools 据此展示页面标题栏地址。
	targetURL      = securityOrigin + "/"
	securityOrigin = "http://mrp.invalid"
	// frameID / loaderID 固定沿用同一标识，满足 CDP 事件必填字段。
	frameID  = "mrp-frame"
	loaderID = "mrp-loader"
)

// Server 承载 CDP 发现端点的 HTTP 服务：/json/version 与 /json/list 供 chrome://inspect 发现，
// /devtools/page/mrp 提供 WebSocket 会话并推送 Network 域事件。
type Server struct {
	session  *capture.Session
	addr     string
	listener net.Listener
	server   *http.Server
	upgrader websocket.Upgrader
	hub      *hub
}

// New 构建服务实例，addr 为监听地址（host:port），由 Start 绑定。
func New(session *capture.Session, addr string) *Server {
	instance := &Server{
		session: session,
		addr:    addr,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", instance.serveVersion)
	mux.HandleFunc("/json/list", instance.serveList)
	mux.HandleFunc("/json", instance.serveList)
	mux.HandleFunc("/devtools/page/"+pageID, instance.servePage)
	instance.server = &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout}
	instance.hub = newHub(session)
	return instance
}

// Addr 返回监听地址：Start 成功后为实际绑定地址，失败时为请求地址，便于启动阶段日志定位。
func (s *Server) Addr() string { return s.addr }

// Start 绑定监听地址并异步服务，返回绑定错误（同步，供启动阶段快速失败）。
func (s *Server) Start() error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.addr = listener.Addr().String()
	s.listener = listener
	go func() { _ = s.server.Serve(listener) }()
	go s.hub.run()
	return nil
}

// Close 停止后台广播并关闭监听，令服务退出。
func (s *Server) Close() error {
	s.hub.stop()
	if s.listener == nil {
		return nil
	}
	return s.listener.Close()
}

// serveVersion 返回 CDP 版本信息，chrome://inspect 靠它确认端点为可发现的调试目标。
func (s *Server) serveVersion(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"Browser":              "mrp/1.0.0",
		"Protocol-Version":     "1.3",
		"User-Agent":           "mrp",
		"webSocketDebuggerUrl": s.webSocketURL(request),
	})
}

// serveList 返回单一聚合目标，供 chrome://inspect 点击 inspect 后连接 WebSocket。
func (s *Server) serveList(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode([]map[string]any{{
		"id":                   pageID,
		"type":                 "page",
		"title":                "mrp proxy traffic",
		"url":                  targetURL,
		"description":          "mrp MITM proxy traffic",
		"webSocketDebuggerUrl": s.webSocketURL(request),
	}})
}

// webSocketURL 依据请求的 Host 动态构造 WebSocket 地址，确保客户端按其实际访问地址回连。
func (s *Server) webSocketURL(request *http.Request) string {
	return "ws://" + request.Host + "/devtools/page/" + pageID
}

// servePage 升级为 WebSocket 进入 CDP 会话。
func (s *Server) servePage(writer http.ResponseWriter, request *http.Request) {
	conn, err := s.upgrader.Upgrade(writer, request, nil)
	if err != nil {
		return
	}
	s.hub.serve(conn)
}

// hub 汇总所有 CDP 会话连接，单一轮询会话新增记录并广播，multi-connection 共享一份拉取。
type hub struct {
	session *capture.Session
	mu      sync.Mutex
	subs    map[*conn]struct{}
	after   uint64
	stopCh  chan struct{}
}

func newHub(session *capture.Session) *hub {
	return &hub{
		session: session,
		subs:    make(map[*conn]struct{}),
		stopCh:  make(chan struct{}),
	}
}

// run 周期拉取会话新增记录并广播给所有连接，直到 stop 关闭。
func (h *hub) run() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
		}
		records, latest := h.session.Tail(h.since())
		h.advance(latest)
		if len(records) == 0 {
			continue
		}
		h.broadcast(records)
	}
}

// since 返回广播游标快照，避免与 register 的写入构成数据竞争。
func (h *hub) since() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.after
}

// advance 将全局游标推进到 seq（只增不减），避免历史重复广播。
func (h *hub) advance(seq uint64) {
	h.mu.Lock()
	if seq > h.after {
		h.after = seq
	}
	h.mu.Unlock()
}

// broadcast 把一批记录编码为 Network 事件广播给全部连接。
func (h *hub) broadcast(records []capture.Record) {
	events := eventsForRecords(records)
	h.mu.Lock()
	subs := make([]*conn, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	h.mu.Unlock()
	for _, sub := range subs {
		sub.enqueueEvents(events)
	}
}

// register 把连接加入订阅集合，并把全局游标推进到该连接已转储的历史序号。
func (h *hub) register(c *conn, latest uint64) {
	h.mu.Lock()
	h.subs[c] = struct{}{}
	if latest > h.after {
		h.after = latest
	}
	h.mu.Unlock()
}

// unregister 从订阅集合移除连接。
func (h *hub) unregister(c *conn) {
	h.mu.Lock()
	delete(h.subs, c)
	h.mu.Unlock()
}

// stop 停止轮询并关闭全部连接。
func (h *hub) stop() {
	select {
	case <-h.stopCh:
		return
	default:
		close(h.stopCh)
	}
	h.mu.Lock()
	for sub := range h.subs {
		sub.close()
		_ = sub.ws.Close()
	}
	h.mu.Unlock()
}

// serve 运行单个 CDP 会话：读请求、处理命令、回写响应与事件，直到客户端断开。
func (h *hub) serve(ws *websocket.Conn) {
	c := &conn{
		session: h.session,
		ws:      ws,
		send:    make(chan []byte, sendBuffer),
		closeCh: make(chan struct{}),
	}
	readDone := make(chan struct{})
	writeDone := make(chan struct{})
	go func() { defer close(readDone); c.readLoop(h) }()
	go func() { defer close(writeDone); c.writeLoop() }()
	<-readDone
	c.close()
	<-writeDone
	h.unregister(c)
	_ = ws.Close()
}

// conn 单个 WebSocket CDP 会话，读写各占一线程以遵守 gorilla 并发约束。
type conn struct {
	session *capture.Session
	ws      *websocket.Conn
	send    chan []byte
	closeCh chan struct{}
	once    sync.Once
}

// rpcRequest CDP 命令报文，仅取 id 与 method，params 留待按需解析。
type rpcRequest struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// readLoop 读取并分派 CDP 命令，连接断开后退出。
func (c *conn) readLoop(h *hub) {
	for {
		_, message, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		c.dispatch(h, message)
	}
}

// writeLoop 串行写入响应与事件，监听 closeCh 以便及时退出。
func (c *conn) writeLoop() {
	for {
		select {
		case payload := <-c.send:
			_ = c.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := c.ws.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		case <-c.closeCh:
			return
		}
	}
}

func (c *conn) close() {
	c.once.Do(func() { close(c.closeCh) })
}

// dispatch 处理单条 CDP 命令：Network.enable 转储历史并订阅，getResponseBody 回传响应体，其余回空结果。
func (c *conn) dispatch(h *hub, message []byte) {
	var request rpcRequest
	if err := json.Unmarshal(message, &request); err != nil || request.Method == "" {
		return
	}
	switch request.Method {
	case "Network.enable":
		c.handleNetworkEnable(h, request)
	case "Network.getResponseBody":
		c.handleGetResponseBody(request)
	case "Target.getTargetInfo":
		c.reply(request.ID, map[string]any{"targetInfo": targetInfo()})
	case "Page.getFrameTree", "Page.getResourceTree":
		c.reply(request.ID, map[string]any{"frameTree": frameTree()})
	default:
		c.reply(request.ID, map[string]any{})
	}
}

// handleNetworkEnable 先订阅并应答，再回放会话现存历史与帧导航事件，确保不漏后续实时记录。
func (c *conn) handleNetworkEnable(h *hub, request rpcRequest) {
	records, latest := c.session.Tail(0)
	h.register(c, latest)
	c.reply(request.ID, map[string]any{})
	c.enqueueEvents(append([][]byte{frameNavigated()}, eventsForRecords(records)...))
}

// targetInfo 返回 CDP 目标信息，供 DevTools 前端确认自身已附加的页面上下文。
func targetInfo() map[string]any {
	return map[string]any{
		"targetId": pageID,
		"type":     "page",
		"title":    "mrp proxy traffic",
		"url":      targetURL,
		"attached": true,
	}
}

// frameTree 返回单帧结构，使网络面板把请求归属到 mrp 这一帧。
func frameTree() map[string]any {
	return map[string]any{
		"frame":       frameInfo(),
		"childFrames": []any{},
	}
}

// frameInfo 返回帧标识与来源信息。
func frameInfo() map[string]any {
	return map[string]any{
		"id":             frameID,
		"loaderId":       loaderID,
		"url":            targetURL,
		"securityOrigin": securityOrigin,
	}
}

// frameNavigated 发送帧导航事件，建立帧与 URL 的归属关系。
func frameNavigated() []byte {
	payload, _ := json.Marshal(map[string]any{
		"method": "Page.frameNavigated",
		"params": map[string]any{
			"frame": frameInfo(),
			"type":  "Navigation",
		},
	})
	return payload
}

// handleGetResponseBody 按 requestId（即序号）回传响应体，二进制按 base64 约定标注。
func (c *conn) handleGetResponseBody(request rpcRequest) {
	var params struct {
		RequestID string `json:"requestId"`
	}
	_ = json.Unmarshal(request.Params, &params)
	result := map[string]any{"body": "", "base64Encoded": false}
	if seq, err := strconv.ParseUint(params.RequestID, 10, 64); err == nil {
		if entry, ok := c.session.EntryBySeq(seq); ok {
			result["body"] = entry.Response.Content.Text
			result["base64Encoded"] = entry.Response.Content.Encoding == "base64"
		}
	}
	c.reply(request.ID, result)
}

// reply 编码一条命令响应入队。
func (c *conn) reply(id int, result any) {
	payload, err := json.Marshal(map[string]any{"id": id, "result": result})
	if err != nil {
		return
	}
	select {
	case c.send <- payload:
	default:
	}
}

// enqueueEvents 将一批事件入队，队列满则丢弃保持非阻塞。
func (c *conn) enqueueEvents(events [][]byte) {
	for _, event := range events {
		select {
		case c.send <- event:
		default:
		}
	}
}

// eventsForRecords 把每条交互记录编码为 Network.requestWillBeSent / responseReceived / loadingFinished 三个事件。
func eventsForRecords(records []capture.Record) [][]byte {
	events := make([][]byte, 0, len(records)*3)
	for _, record := range records {
		timestamp := harTime(record.Entry)
		events = append(events,
			requestWillBeSent(record.Seq, record.Entry, timestamp),
			responseReceived(record.Seq, record.Entry, timestamp),
			loadingFinished(record.Seq, record.Entry, timestamp),
		)
	}
	return events
}

// requestWillBeSent 发送请求发出事件。
func requestWillBeSent(seq uint64, entry capture.Entry, timestamp float64) []byte {
	requestID := strconv.FormatUint(seq, 10)
	request := map[string]any{
		"url":     entry.Request.URL,
		"method":  entry.Request.Method,
		"headers": headersMap(entry.Request.Headers),
	}
	hasPostData := entry.Request.PostData != nil
	if entry.Request.PostData != nil {
		text := entry.Request.PostData.Text
		if entry.Request.PostData.Encoding == "base64" {
			text = decodeBase64(text)
		}
		request["postData"] = text
	}
	request["hasPostData"] = hasPostData
	payload, _ := json.Marshal(map[string]any{
		"method": "Network.requestWillBeSent",
		"params": map[string]any{
			"requestId": requestID,
			"loaderId":  loaderID,
			"frameId":   frameID,
			"timestamp": timestamp,
			"wallTime":  timestamp,
			"initiator": map[string]any{"type": "other"},
			"type":      "Fetch",
			"request":   request,
		},
	})
	return payload
}

// responseReceived 发送响应到达事件。
func responseReceived(seq uint64, entry capture.Entry, timestamp float64) []byte {
	requestID := strconv.FormatUint(seq, 10)
	payload, _ := json.Marshal(map[string]any{
		"method": "Network.responseReceived",
		"params": map[string]any{
			"requestId": requestID,
			"loaderId":  loaderID,
			"timestamp": timestamp,
			"type":      "Fetch",
			"response": map[string]any{
				"url":               entry.Request.URL,
				"status":            entry.Response.Status,
				"statusText":        entry.Response.StatusText,
				"headers":           headersMap(entry.Response.Headers),
				"mimeType":          entry.Response.Content.MimeType,
				"encodedDataLength": entry.Response.BodySize,
				"protocol":          entry.Response.HTTPVersion,
				"connectionReused":  false,
				"remoteIPAddress":   entry.ServerIPAddress,
			},
		},
	})
	return payload
}

// loadingFinished 发送响应加载完成事件。
func loadingFinished(seq uint64, entry capture.Entry, timestamp float64) []byte {
	requestID := strconv.FormatUint(seq, 10)
	payload, _ := json.Marshal(map[string]any{
		"method": "Network.loadingFinished",
		"params": map[string]any{
			"requestId":         requestID,
			"timestamp":         timestamp,
			"encodedDataLength": entry.Response.BodySize,
		},
	})
	return payload
}

// headersMap 把 HAR 头列表折叠为 CDP 需要的键值映射，同名多值取末位（最小实现）。
func headersMap(headers []capture.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for _, header := range headers {
		out[header.Name] = header.Value
	}
	return out
}

// harTime 解析 HAR 起始时间戳为 Unix 秒，失败回退当前时间。
func harTime(entry capture.Entry) float64 {
	t, err := time.Parse(harTimeLayout, entry.StartedDateTime)
	if err != nil {
		return float64(time.Now().UnixNano()) / 1e9
	}
	return float64(t.UnixNano()) / 1e9
}

// decodeBase64 解码失败时返回原文，避免二进制体破坏请求事件。
func decodeBase64(text string) string {
	decoded, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return text
	}
	return string(decoded)
}
