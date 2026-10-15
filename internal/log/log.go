// Package log 输出「时间\t级别\t消息」三段式控制台日志，按级别过滤并按级别着色。
// 消息以小写事件名开头，上下文以内嵌 key=value 表达，键名统一 snake_case，
// 失败原因置于末尾并以前置冒号与原因正文分隔。
package log

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
	DebugLevel Level = iota
	InfoLevel
	WarnLevel
	ErrorLevel
)

const logTimeLayout = "01-02 15:04:05.000"

var (
	logOut   io.Writer = os.Stdout
	logFloor Level     = InfoLevel
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

func Debug(format string, args ...any) { logf(DebugLevel, format, args...) }
func Info(format string, args ...any)  { logf(InfoLevel, format, args...) }
func Warn(format string, args ...any)  { logf(WarnLevel, format, args...) }
func Error(format string, args ...any) { logf(ErrorLevel, format, args...) }

// Fatal 输出 ERROR 日志后终止进程，仅供启动期不可恢复失败使用，请求热路径禁止调用。
func Fatal(format string, args ...any) {
	logf(ErrorLevel, format, args...)
	os.Exit(1)
}

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
			line = code + line + "\x1b[0m"
		}
	}
	return line + "\n"
}

func (l Level) String() string {
	switch l {
	case DebugLevel:
		return "DEBUG"
	case InfoLevel:
		return "INFO"
	case WarnLevel:
		return "WARN"
	case ErrorLevel:
		return "ERROR"
	}
	return "UNKNOWN"
}

func levelColor(level Level) string {
	// 级别 ANSI 色：DEBUG 灰、INFO 普通色、WARN 黄、ERROR 红
	switch level {
	case DebugLevel:
		return "\x1b[90m"
	case WarnLevel:
		return "\x1b[33m"
	case ErrorLevel:
		return "\x1b[31m"
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
	for level := DebugLevel; level <= ErrorLevel; level++ {
		if strings.EqualFold(text, level.String()) {
			return level, nil
		}
	}
	return InfoLevel, fmt.Errorf("--log %q: must be debug, info, warn or error", text)
}
