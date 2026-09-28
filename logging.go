package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
)

const consoleTimeLayout = "01-02 15:04:05.000"

// 级别着色按行业通用规则：DEBUG 灰、INFO 绿、WARN 黄、ERROR 红
const (
	colorDebug = "\x1b[90m"
	colorInfo  = "\x1b[32m"
	colorWarn  = "\x1b[33m"
	colorError = "\x1b[31m"
	colorReset = "\x1b[0m"
)

type consoleHandler struct {
	out    io.Writer
	level  slog.Leveler
	color  bool
	mu     *sync.Mutex
	attrs  []string
	prefix string
}

func newConsoleHandler(out io.Writer, level slog.Leveler) *consoleHandler {
	return &consoleHandler{
		out:   out,
		level: level,
		color: isTerminal(out),
		mu:    &sync.Mutex{},
	}
}

func isTerminal(out io.Writer) bool {
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *consoleHandler) Handle(_ context.Context, record slog.Record) error {
	parts := make([]string, 0, len(h.attrs)+record.NumAttrs())
	parts = append(parts, h.attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		if s := renderAttr(h.prefix, attr); s != "" {
			parts = append(parts, s)
		}
		return true
	})

	levelText := record.Level.String()
	if h.color {
		levelText = levelColor(record.Level) + levelText + colorReset
	}

	var line strings.Builder
	line.WriteString(record.Time.Format(consoleTimeLayout))
	line.WriteByte('\t')
	line.WriteString(levelText)
	line.WriteByte('\t')
	line.WriteString(record.Message)
	if len(parts) > 0 {
		line.WriteByte(' ')
		line.WriteString(strings.Join(parts, " "))
	}
	line.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, line.String())
	return err
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &consoleHandler{out: h.out, level: h.level, color: h.color, mu: h.mu, prefix: h.prefix}
	next.attrs = make([]string, 0, len(h.attrs)+len(attrs))
	next.attrs = append(next.attrs, h.attrs...)
	for _, attr := range attrs {
		if s := renderAttr(h.prefix, attr); s != "" {
			next.attrs = append(next.attrs, s)
		}
	}
	return next
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &consoleHandler{out: h.out, level: h.level, color: h.color, mu: h.mu, attrs: h.attrs, prefix: h.prefix + name + "."}
}

func levelColor(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return colorDebug
	case level < slog.LevelWarn:
		return colorInfo
	case level < slog.LevelError:
		return colorWarn
	default:
		return colorError
	}
}

func renderAttr(prefix string, attr slog.Attr) string {
	key := prefix + attr.Key
	if attr.Value.Kind() == slog.KindGroup {
		group := attr.Value.Group()
		if len(group) == 0 {
			return ""
		}
		parts := make([]string, 0, len(group))
		for _, sub := range group {
			parts = append(parts, renderAttr(key+".", sub))
		}
		return strings.Join(parts, " ")
	}
	return key + "=" + renderValue(attr.Value)
}

func renderValue(value slog.Value) string {
	text := value.String()
	if needsQuote(text) {
		return strconv.Quote(text)
	}
	return text
}

func needsQuote(text string) bool {
	if text == "" {
		return true
	}
	for _, r := range text {
		if r <= ' ' || r == '"' || r == '=' || r == 0x7f {
			return true
		}
	}
	return false
}
