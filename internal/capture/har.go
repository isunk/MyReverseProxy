package capture

// Har HAR 1.2 文档根对象，顶层仅含 log 键。
type Har struct {
	Log Log `json:"log"`
}

// Log 一条抓包归档的元信息与 entries 列表，详见 http://www.softwareishard.com/blog/har-12-spec/。
type Log struct {
	Version string  `json:"version"`
	Creator Creator `json:"creator"`
	Pages   []Page  `json:"pages"`
	Entries []Entry `json:"entries"`
}

// Creator 生成 HAR 的工具标识。
type Creator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Page 页面维度在 mrp 场景未使用，保留占位类型以满足 HAR 规范字段。
type Page struct{}

// Entry 一条完整的请求/响应交互记录。
type Entry struct {
	StartedDateTime string   `json:"startedDateTime"`
	Time            float64  `json:"time"`
	Request         Request  `json:"request"`
	Response        Response `json:"response"`
	Cache           Cache    `json:"cache"`
	Timings         Timings  `json:"timings"`
	ServerIPAddress string   `json:"serverIPAddress,omitempty"`
	ServerName      string   `json:"_serverName,omitempty"`
	ClientAddress   string   `json:"_clientAddress,omitempty"`
}

// Request 客户端实际发出的请求字段。
type Request struct {
	Method      string    `json:"method"`
	URL         string    `json:"url"`
	HTTPVersion string    `json:"httpVersion"`
	Cookies     []Cookie  `json:"cookies"`
	Headers     []Header  `json:"headers"`
	QueryString []Query   `json:"queryString"`
	PostData    *PostData `json:"postData,omitempty"`
	HeadersSize int       `json:"headersSize"`
	BodySize    int64     `json:"bodySize"`
}

// Response 回传客户端的响应字段。
type Response struct {
	Status      int      `json:"status"`
	StatusText  string   `json:"statusText"`
	HTTPVersion string   `json:"httpVersion"`
	Cookies     []Cookie `json:"cookies"`
	Headers     []Header `json:"headers"`
	Content     Content  `json:"content"`
	RedirectURL string   `json:"redirectURL"`
	HeadersSize int      `json:"headersSize"`
	BodySize    int64    `json:"bodySize"`
}

// Header 单个消息头键值对，同名多值头逐条展开。
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Cookie 占位类型，mrp 不解析 Cookie 明细。
type Cookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Query 单个查询参数，mrp 不展开 query 明细。
type Query struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// PostData 请求体：Text 为记入文件的前缀字节（二进制时 base64 编码），BodySize 在外层 Request 记录原始大小。
type PostData struct {
	MimeType  string `json:"mimeType,omitempty"`
	Text      string `json:"text,omitempty"`
	Encoding  string `json:"_encoding,omitempty"`
	Truncated bool   `json:"_truncated,omitempty"`
}

// Content 响应体：Size 为记入文件的前缀字节数，Text 在二进制内容时 base64 编码并以 Encoding 标注，Truncated 标注是否被截断。
type Content struct {
	Size      int    `json:"size"`
	MimeType  string `json:"mimeType,omitempty"`
	Text      string `json:"text,omitempty"`
	Encoding  string `json:"encoding,omitempty"`
	Truncated bool   `json:"_truncated,omitempty"`
}

// Cache HAR 规范要求但 mrp 不使用的字段，保留空对象。
type Cache struct{}

// Timings 耗时分解：mrp 仅记录总的 wait，其余置零。
type Timings struct {
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
}
