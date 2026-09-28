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

// 级别名与 ANSI 色按同一索引对应：DEBUG 灰、INFO 绿、WARN 黄、ERROR 红
var (
	levelNames  = [...]string{"DEBUG", "INFO", "WARN", "ERROR"}
	levelColors = [...]string{"\x1b[90m", "\x1b[32m", "\x1b[33m", "\x1b[31m"}
	colorReset  = "\x1b[0m"
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
	levelText := levelNames[level]
	if color {
		levelText = levelColors[level] + levelText + colorReset
	}
	return now.Format(logTimeLayout) + "\t" + levelText + "\t" + message + "\n"
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func parseLogLevel(text string) (logLevel, error) {
	for level, name := range levelNames {
		if strings.EqualFold(text, name) {
			return logLevel(level), nil
		}
	}
	return logInfo, fmt.Errorf("unknown log level %q", text)
}
