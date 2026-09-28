package main

import (
	"fmt"
	"io"
	"os"
	"strings"
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

const logTimeLayout = "01-02 15:04:05.000"

// 级别 ANSI 色按行业通用规则：DEBUG 灰、INFO 绿、WARN 黄、ERROR 红
const (
	colorDebug = "\x1b[90m"
	colorInfo  = "\x1b[32m"
	colorWarn  = "\x1b[33m"
	colorError = "\x1b[31m"
	colorReset = "\x1b[0m"
)

var (
	logOut   io.Writer = os.Stdout
	logFloor logLevel  = logInfo
	logColor bool
	logMu    sync.Mutex
)

func initLogging(level logLevel) {
	logFloor = level
	logColor = isTerminal(os.Stdout)
}

func logDebugf(format string, args ...any) { logf(logDebug, format, args...) }
func logInfof(format string, args ...any)  { logf(logInfo, format, args...) }
func logWarnf(format string, args ...any)  { logf(logWarn, format, args...) }
func logErrorf(format string, args ...any) { logf(logError, format, args...) }

func logf(level logLevel, format string, args ...any) {
	if level < logFloor {
		return
	}
	line := formatLogLine(time.Now(), level, fmt.Sprintf(format, args...), logColor)
	logMu.Lock()
	defer logMu.Unlock()
	_, _ = io.WriteString(logOut, line)
}

func formatLogLine(now time.Time, level logLevel, message string, color bool) string {
	line := now.Format(logTimeLayout) + "\t" + level.String() + "\t" + message
	if color {
		line = levelColor(level) + line + colorReset
	}
	return line + "\n"
}

func (l logLevel) String() string {
	switch l {
	case logDebug:
		return "DEBUG"
	case logInfo:
		return "INFO"
	case logWarn:
		return "WARN"
	case logError:
		return "ERROR"
	default:
		return "INFO"
	}
}

func levelColor(level logLevel) string {
	switch level {
	case logDebug:
		return colorDebug
	case logInfo:
		return colorInfo
	case logWarn:
		return colorWarn
	case logError:
		return colorError
	default:
		return colorInfo
	}
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func parseLogLevel(text string) (logLevel, error) {
	for level := logDebug; level <= logError; level++ {
		if strings.EqualFold(text, level.String()) {
			return level, nil
		}
	}
	return logInfo, fmt.Errorf("unknown log level %q", text)
}
