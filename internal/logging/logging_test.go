package logging

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

var testStamp = time.Date(2026, 9, 28, 9, 21, 36, 865_000_000, time.FixedZone("CST", 8*3600))

func TestFormatLogLine(t *testing.T) {
	got := formatLogLine(testStamp, Info, "created default config file path=config.yaml", false)
	want := "09-28 09:21:36.865\tINFO\tcreated default config file path=config.yaml\n"
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
		{Debug, "\x1b[90m", true},
		{Info, "", false},
		{Warn, "\x1b[33m", true},
		{Error, "\x1b[31m", true},
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
		"debug": Debug,
		"info":  Info,
		"warn":  Warn,
		"error": Error,
		"DEBUG": Debug,
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
	logOut, logFloor, logColor = &out, Warn, false
	t.Cleanup(func() {
		logOut, logFloor, logColor = io.Discard, Info, false
	})

	Infof("hidden")
	Warnf("shown")
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

	Infof("event name=%s", "mrp")
	if !strings.Contains(out.String(), "event name=mrp") {
		t.Fatalf("SetOutput not effective: %q", out.String())
	}
}
