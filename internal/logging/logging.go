package logging

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	Debug Level = iota
	Info
	Warn
	Error
)

const logTimeLayout = "01-02 15:04:05.000"

// 级别 ANSI 色：DEBUG 灰、INFO 普通色、WARN 黄、ERROR 红
const (
	colorDebug = "\x1b[90m"
	colorWarn  = "\x1b[33m"
	colorError = "\x1b[31m"
	colorReset = "\x1b[0m"
)

var (
	logOut   io.Writer = os.Stdout
	logFloor Level     = Info
	logColor bool
	logMu    sync.Mutex
)

// Init 设定级别过滤与着色策略，着色仅在 stdout 为终端时启用。
func Init(level Level) {
	logFloor = level
	logColor = isTerminal(os.Stdout)
}

// SetOutput 替换日志输出目标，测试用于丢弃输出或捕获内容。
func SetOutput(writer io.Writer) {
	logOut = writer
}

func Debugf(format string, args ...any) { logf(Debug, format, args...) }
func Infof(format string, args ...any)  { logf(Info, format, args...) }
func Warnf(format string, args ...any)  { logf(Warn, format, args...) }
func Errorf(format string, args ...any) { logf(Error, format, args...) }

func logf(level Level, format string, args ...any) {
	if level < logFloor {
		return
	}
	line := formatLogLine(time.Now(), level, fmt.Sprintf(format, args...), logColor)
	logMu.Lock()
	defer logMu.Unlock()
	_, _ = io.WriteString(logOut, line)
}

func formatLogLine(now time.Time, level Level, message string, color bool) string {
	line := now.Format(logTimeLayout) + "\t" + level.String() + "\t" + message
	if color {
		if code := levelColor(level); code != "" {
			line = code + line + colorReset
		}
	}
	return line + "\n"
}

func (l Level) String() string {
	switch l {
	case Debug:
		return "DEBUG"
	case Info:
		return "INFO"
	case Warn:
		return "WARN"
	case Error:
		return "ERROR"
	}
	return "UNKNOWN"
}

func levelColor(level Level) string {
	switch level {
	case Debug:
		return colorDebug
	case Warn:
		return colorWarn
	case Error:
		return colorError
	default:
		return ""
	}
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// ParseLevel 解析命令行给出的级别名，大小写不敏感，失败时报错。
func ParseLevel(text string) (Level, error) {
	for level := Debug; level <= Error; level++ {
		if strings.EqualFold(text, level.String()) {
			return level, nil
		}
	}
	return Info, fmt.Errorf("unknown log level %q", text)
}
