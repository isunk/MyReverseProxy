package main

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

type logLevel int

const (
	logDebug logLevel = iota
	logInfo
	logWarn
	logError
)

func (l logLevel) String() string {
	switch l {
	case logDebug:
		return "DEBUG"
	case logWarn:
		return "WARN"
	case logError:
		return "ERROR"
	default:
		return "INFO"
	}
}

// 级别着色按行业通用规则：DEBUG 灰、INFO 绿、WARN 黄、ERROR 红
const (
	colorDebug = "\x1b[90m"
	colorInfo  = "\x1b[32m"
	colorWarn  = "\x1b[33m"
	colorError = "\x1b[31m"
	colorReset = "\x1b[0m"
)

var logState = struct {
	sync.Mutex
	out   io.Writer
	min   logLevel
	color bool
}{out: os.Stdout, min: logInfo}

func initLogging(level logLevel) {
	logState.min = level
	logState.color = isTerminal(os.Stdout)
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func parseLogLevel(text string) (logLevel, error) {
	switch text {
	case "debug":
		return logDebug, nil
	case "info":
		return logInfo, nil
	case "warn":
		return logWarn, nil
	case "error":
		return logError, nil
	}
	return logInfo, fmt.Errorf("unknown log level %q", text)
}

func logDebugf(format string, args ...any) { logf(logDebug, format, args...) }
func logInfof(format string, args ...any)  { logf(logInfo, format, args...) }
func logWarnf(format string, args ...any)  { logf(logWarn, format, args...) }
func logErrorf(format string, args ...any) { logf(logError, format, args...) }

func logf(level logLevel, format string, args ...any) {
	if level < logState.min {
		return
	}
	line := formatLogLine(time.Now(), level, fmt.Sprintf(format, args...), logState.color)
	logState.Lock()
	defer logState.Unlock()
	_, _ = io.WriteString(logState.out, line)
}

func formatLogLine(now time.Time, level logLevel, message string, color bool) string {
	levelText := level.String()
	if color {
		levelText = levelColor(level) + levelText + colorReset
	}
	return now.Format("01-02 15:04:05.000") + "\t" + levelText + "\t" + message + "\n"
}

func levelColor(level logLevel) string {
	switch level {
	case logDebug:
		return colorDebug
	case logWarn:
		return colorWarn
	case logError:
		return colorError
	default:
		return colorInfo
	}
}
