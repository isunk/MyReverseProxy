package log

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

var testStamp = time.Date(2026, 9, 28, 9, 21, 36, 865_000_000, time.FixedZone("CST", 8*3600))

func TestFormatLogLine(t *testing.T) {
	got := formatLogLine(testStamp, InfoLevel, "config created path=mrp.yaml", false)
	want := "09-28 09:21:36.865\tINFO\tconfig created path=mrp.yaml\n"
	if got != want {
		t.Fatalf("format mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestFormatLogLine_Color(t *testing.T) {
	for _, tc := range []struct {
		level Level
		code  string
		reset bool
	}{
		{DebugLevel, "\x1b[90m", true},
		{InfoLevel, "", false},
		{WarnLevel, "\x1b[33m", true},
		{ErrorLevel, "\x1b[31m", true},
	} {
		got := formatLogLine(testStamp, tc.level, "event", true)
		want := tc.code + "09-28 09:21:36.865\t" + tc.level.String() + "\tevent"
		if tc.reset {
			want += "\x1b[0m"
		}
		want += "\n"
		if got != want {
			t.Fatalf("level %v color mismatch:\n got %q\nwant %q", tc.level, got, want)
		}
	}
}

func TestParseLevel(t *testing.T) {
	for text, want := range map[string]Level{
		"debug": DebugLevel,
		"info":  InfoLevel,
		"warn":  WarnLevel,
		"error": ErrorLevel,
		"DEBUG": DebugLevel,
	} {
		got, err := ParseLevel(text)
		if err != nil || got != want {
			t.Fatalf("ParseLevel(%q) = %v, %v", text, got, err)
		}
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Fatal("unknown level should fail")
	}
}

func TestLogf_LevelFilter(t *testing.T) {
	var out bytes.Buffer
	Init(WarnLevel)
	SetOutput(&out)
	t.Cleanup(func() {
		Init(InfoLevel)
		SetOutput(io.Discard)
	})

	Info("hidden")
	Warn("shown")
	text := out.String()
	// 只断言级别过滤结果，不重算时间戳：期望值用 time.Now() 会跨毫秒而与实际日志不一致
	if strings.Contains(text, "hidden") {
		t.Fatalf("info message leaked above the warn floor: %q", text)
	}
	if !strings.Contains(text, "\tWARN\tshown") {
		t.Fatalf("warn message was dropped by the warn floor: %q", text)
	}
}

func TestSetOutput(t *testing.T) {
	var out bytes.Buffer
	SetOutput(&out)
	t.Cleanup(func() { SetOutput(io.Discard) })

	Info("event name=%s", "mrp")
	if !strings.Contains(out.String(), "event name=mrp") {
		t.Fatalf("SetOutput not effective: %q", out.String())
	}
}

// TestSetOutputConcurrentWithLogging 并发替换输出配置与日志输出，-race 下验证无数据竞争
func TestSetOutputConcurrentWithLogging(t *testing.T) {
	t.Cleanup(func() {
		Init(InfoLevel)
		SetOutput(io.Discard)
	})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			Info("concurrent probe %d", i)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			SetOutput(io.Discard)
			SetOutput(&bytes.Buffer{})
			Init(WarnLevel)
			Init(InfoLevel)
		}
	}()
	wg.Wait()
}
