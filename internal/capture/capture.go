// Package capture 把代理明文交互增量记录为 HAR 1.2 文件。
package capture

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
)

const (
	creatorName    = "mrp"
	creatorVersion = "1.0.0"

	FileSizeLimit = 256 << 20 // 抓包文件累计写入上限，达到后停止追加
)

// header 是打开文件时写入的前端：entries 数组随后由 Record 增量填充，末尾由 Close 补全。
const header = `{"log":{"version":"1.2","creator":{"name":"` + creatorName + `","version":"` + creatorVersion + `"},"pages":[],"entries":[`

// ErrLimitReached 表示文件累计达到 limit，由 Record 返回，调用方据此告警一次；此后 Record 短路返回 nil。
var ErrLimitReached = errors.New("capture file size limit reached")

// Recorder 并发写盘：json.Marshal 在锁外完成，mu 仅串行化写盘，stopped 为 atomic 供锁外短路检查。
type Recorder struct {
	file     *os.File
	writer   *bufio.Writer
	path     string
	mu       sync.Mutex
	wroteAny bool
	bytes    int64
	limit    int64
	stopped  atomic.Bool
}

// New 创建（或截断）输出文件并写 HAR 头部；创建失败返回 error，由调用方降级为抓包关闭。
func New(path string, limit int64) (*Recorder, error) {
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	recorder := &Recorder{file: file, writer: bufio.NewWriter(file), path: path, limit: limit}
	if _, err := recorder.writer.WriteString(header); err != nil {
		file.Close()
		return nil, err
	}
	recorder.bytes = int64(len(header))
	return recorder, nil
}

// Path 返回输出文件路径，供调用方在告警日志中定位。
func (r *Recorder) Path() string { return r.path }

// Record 追加一条 entry：json.Marshal 在锁外完成，持锁串行写盘；达 limit 或写失败后停用。
func (r *Recorder) Record(entry Entry) error {
	if r.stopped.Load() {
		return nil
	}
	data, err := json.Marshal(entry)
	if err != nil {
		r.stopped.Store(true)
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped.Load() {
		return nil
	}
	written := int64(len(data))
	if r.wroteAny {
		if err := r.writer.WriteByte(','); err != nil {
			r.stopped.Store(true)
			return err
		}
		written++
	}
	if _, err := r.writer.Write(data); err != nil {
		r.stopped.Store(true)
		return err
	}
	r.wroteAny = true
	r.bytes += written
	if r.limit > 0 && r.bytes >= r.limit {
		r.stopped.Store(true)
		return ErrLimitReached
	}
	return nil
}

// Close 补全尾部并关闭文件；幂等，停用后仍能补尾，置位 stopped 使后续 Record 短路。
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	r.stopped.Store(true)
	if _, err := r.writer.WriteString(`]}}`); err != nil {
		r.file.Close()
		r.file = nil
		return err
	}
	if err := r.writer.Flush(); err != nil {
		r.file.Close()
		r.file = nil
		return err
	}
	err := r.file.Close()
	r.file = nil
	return err
}
