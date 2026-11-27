package capture

import "sync"

const (
	// MaxEntries 内存会话默认保留的最近交互条数上限，超出后最旧记录被丢弃。
	MaxEntries = 5000
	// MaxBytes 内存会话累计 body 字节上限，超出后按最旧丢弃，避免长期运行内存无界增长。
	MaxBytes = 64 << 20
)

// sessionItem 环形槽位：记录 seq（全局单调序号）、entry 及其近似字节占用，供超预算按最旧回收与增量订阅定位。
type sessionItem struct {
	seq   uint64
	entry Entry
	size  int64
}

// Record 一条带全局序号的交互记录快照，Seq 单调递增，供 CDP 等订阅方按序号拉取新增记录。
type Record struct {
	Seq   uint64
	Entry Entry
}

// Session 以环形缓冲保留最近抓包交互，供实时 HAR 导出（Chrome DevTools 原生导入）读取；与文件记录器并列，互不影响。
type Session struct {
	mu       sync.Mutex
	items    []sessionItem
	head     int
	count    int
	limit    int
	bytes    int64
	maxBytes int64
	total    uint64
}

// NewSession 创建内存会话；limit 或 maxBytes 非正时取各自默认上限。
func NewSession(limit int, maxBytes int64) *Session {
	if limit <= 0 {
		limit = MaxEntries
	}
	if maxBytes <= 0 {
		maxBytes = MaxBytes
	}
	return &Session{limit: limit, maxBytes: maxBytes}
}

// Add 追加一条交互记录；达到条数上限覆盖最旧，累计字节超预算时逐条丢弃最旧。每次追加都分配一个单调递增序号。
func (s *Session) Add(entry Entry) {
	size := entrySize(entry)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	if s.items == nil {
		s.items = make([]sessionItem, s.limit)
	}
	if s.count < s.limit {
		s.items[(s.head+s.count)%s.limit] = sessionItem{seq: s.total, entry: entry, size: size}
		s.count++
		s.bytes += size
	} else {
		s.bytes += size - s.items[s.head].size
		s.items[s.head] = sessionItem{seq: s.total, entry: entry, size: size}
		s.head = (s.head + 1) % s.limit
	}
	for s.count > 0 && s.bytes > s.maxBytes {
		s.bytes -= s.items[s.head].size
		s.head = (s.head + 1) % s.limit
		s.count--
	}
}

// Entries 按记录顺序返回全部保留交互的快照。
func (s *Session) Entries() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, s.count)
	for i := 0; i < s.count; i++ {
		out[i] = s.items[(s.head+i)%s.limit].entry
	}
	return out
}

// Tail 返回序号大于 after 且仍存活的全部记录（按序号升序）与当前累计序号；序号较小的记录若已被环形回收则不可再获取。
func (s *Session) Tail(after uint64) ([]Record, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]Record, 0, s.count)
	for i := 0; i < s.count; i++ {
		item := s.items[(s.head+i)%s.limit]
		if item.seq > after {
			records = append(records, Record{Seq: item.seq, Entry: item.entry})
		}
	}
	return records, s.total
}

// EntryBySeq 按序号返回仍存活的交互记录，序号不存在或已回收时 ok 为 false。
func (s *Session) EntryBySeq(seq uint64) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < s.count; i++ {
		item := s.items[(s.head+i)%s.limit]
		if item.seq == seq {
			return item.entry, true
		}
	}
	return Entry{}, false
}

// HAR 把当前保留交互打包为 HAR 1.2 文档，便于直接序列化导出。
func (s *Session) HAR() Har {
	return Har{Log: Log{
		Version: "1.2",
		Creator: Creator{Name: creatorName, Version: creatorVersion},
		Pages:   []Page{},
		Entries: s.Entries(),
	}}
}

// entrySize 估算单条交互占用的内存字节，仅计请求与响应 body 文本（不含头与元数据），用于超预算回收判定。
func entrySize(entry Entry) int64 {
	var size int64
	if entry.Request.PostData != nil {
		size += int64(len(entry.Request.PostData.Text))
	}
	size += int64(len(entry.Response.Content.Text))
	return size
}
