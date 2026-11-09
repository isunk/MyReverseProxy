// Package capture 把代理可见的明文交互记录为 HAR 1.2 文件：Recorder 以增量方式追加 entry，
// 打开时写头部、每条 entry 追加一个 JSON 对象，Close 时补全尾部使文件成为合法 JSON。
package capture

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"sync"
)

const (
	creatorName    = "mrp"
	creatorVersion = "1.0.0"

	// FileSizeLimit 抓包文件累计写入上限，达到后停止追加。
	FileSizeLimit = 256 << 20
)

// header 打开文件时写入的前端，entries 数组随后由 Record 增量填充，末尾由 Close 补全。
const header = `{"log":{"version":"1.2","creator":{"name":"` + creatorName + `","version":"` + creatorVersion + `"},"pages":[],"entries":[`

// ErrLimitReached 文件累计达到 limit 时由 Record 返回，调用方据此告警一次；此后 Record 短路返回 nil。
var ErrLimitReached = errors.New("capture file size limit reached")

// Recorder 序列化并发写盘：mu 串行化 Record 保证单条 entry 完整不交错；写失败或达到 limit 后 stopped 置位，后续 Record 直接短路。
type Recorder struct {
	file     *os.File
	writer   *bufio.Writer
	path     string
	mu       sync.Mutex
	wroteAny bool
	bytes    int64
	limit    int64
	stopped  bool
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

// Record 追加一条 entry：非首条前写逗号，达到 limit 返回 ErrLimitReached 并停用；写盘或序列化失败返回原错误并停用，后续调用短路。
func (r *Recorder) Record(entry Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil
	}
	data, err := json.Marshal(entry)
	if err != nil {
		r.stopped = true
		return err
	}
	if r.wroteAny {
		if err := r.writer.WriteByte(','); err != nil {
			r.stopped = true
			return err
		}
	}
	if _, err := r.writer.Write(data); err != nil {
		r.stopped = true
		return err
	}
	r.wroteAny = true
	r.bytes += int64(len(data))
	if r.limit > 0 && r.bytes >= r.limit {
		r.stopped = true
		return ErrLimitReached
	}
	return nil
}

// Close 补全 entries 数组与根对象尾部、flush 并关闭文件；幂等，停用后仍能补尾。
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
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
