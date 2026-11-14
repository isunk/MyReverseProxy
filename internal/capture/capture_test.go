package capture

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRecorder_WritesValidHAR(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.har")
	recorder, err := New(path, FileSizeLimit)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	entries := []Entry{
		{StartedDateTime: "t1", Request: Request{Method: "GET", URL: "http://a/"}},
		{StartedDateTime: "t2", Request: Request{Method: "POST", URL: "http://b/"}},
	}
	for _, entry := range entries {
		if err := recorder.Record(entry); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var har Har
	if err := json.Unmarshal(data, &har); err != nil {
		t.Fatalf("unmarshal captured file: %v", err)
	}
	if har.Log.Version != "1.2" {
		t.Fatalf("Version = %q, want 1.2", har.Log.Version)
	}
	if har.Log.Creator.Name != "mrp" {
		t.Fatalf("Creator.Name = %q, want mrp", har.Log.Creator.Name)
	}
	if len(har.Log.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(har.Log.Entries))
	}
	if har.Log.Entries[0].Request.URL != "http://a/" || har.Log.Entries[1].Request.URL != "http://b/" {
		t.Fatalf("entry order mismatch: %+v", har.Log.Entries)
	}
}

func TestRecorder_LimitReached(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.har")
	limit := int64(len(header)) + 1
	recorder, err := New(path, limit)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := recorder.Record(Entry{Request: Request{Method: "GET"}}); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("first Record = %v, want ErrLimitReached", err)
	}
	if err := recorder.Record(Entry{Request: Request{Method: "POST"}}); err != nil {
		t.Fatalf("subsequent Record should short-circuit, got %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var har Har
	if err := json.Unmarshal(data, &har); err != nil {
		t.Fatalf("truncated file should remain valid JSON after Close: %v", err)
	}
	if len(har.Log.Entries) != 1 {
		t.Fatalf("entries = %d, want 1 after limit", len(har.Log.Entries))
	}
}

func TestRecorder_CloseIdempotent(t *testing.T) {
	recorder, err := New(filepath.Join(t.TempDir(), "capture.har"), FileSizeLimit)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("second Close should be no-op: %v", err)
	}
}

// TestRecorder_CloseStopsRecord 验证 Close 后 Record 直接短路返回 nil，不再向已关闭的缓冲写入而静默丢数据。
func TestRecorder_CloseStopsRecord(t *testing.T) {
	recorder, err := New(filepath.Join(t.TempDir(), "capture.har"), FileSizeLimit)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := recorder.Record(Entry{Request: Request{Method: "GET", URL: "http://c/"}}); err != nil {
		t.Fatalf("Record after Close should short-circuit: %v", err)
	}
}

func TestRecorder_ConcurrentRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.har")
	recorder, err := New(path, FileSizeLimit)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const count = 200
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := recorder.Record(Entry{Request: Request{Method: "GET", URL: "http://c/"}}); err != nil {
				t.Errorf("Record: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var har Har
	if err := json.Unmarshal(data, &har); err != nil {
		t.Fatalf("concurrent entries should produce valid JSON: %v", err)
	}
	if len(har.Log.Entries) != count {
		t.Fatalf("entries = %d, want %d", len(har.Log.Entries), count)
	}
}

// TestHar_RequiredFieldsPresent 校验生成的 entry 覆盖 HAR 1.2 规范所有必填字段：content.mimeType 与 timings 的 blocked/dns/connect/ssl 必须随 JSON 输出，不得因 omitempty 缺失。
func TestHar_RequiredFieldsPresent(t *testing.T) {
	entry := Entry{
		StartedDateTime: "2026-01-01T00:00:00.000Z",
		Time:            10,
		Request: Request{
			Method:      "GET",
			URL:         "http://a.example.com/",
			HTTPVersion: "HTTP/1.1",
			Cookies:     []Cookie{},
			Headers:     []Header{},
			QueryString: []Query{},
			HeadersSize: -1,
			BodySize:    0,
		},
		Response: Response{
			Status:      200,
			StatusText:  "OK",
			HTTPVersion: "HTTP/1.1",
			Cookies:     []Cookie{},
			Headers:     []Header{},
			Content:     Content{Size: 3, MimeType: "text/plain", Text: "abc"},
			RedirectURL: "",
			HeadersSize: -1,
			BodySize:    3,
		},
		Cache:   Cache{},
		Timings: Timings{Blocked: 0, DNS: 0, Connect: 0, Send: 0, Wait: 10, Receive: 0, SSL: 0},
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	request := root["request"].(map[string]any)
	response := root["response"].(map[string]any)
	content := response["content"].(map[string]any)
	timings := root["timings"].(map[string]any)
	for _, key := range []string{"method", "url", "httpVersion", "cookies", "headers", "queryString", "headersSize", "bodySize"} {
		if _, ok := request[key]; !ok {
			t.Errorf("request.%s missing", key)
		}
	}
	for _, key := range []string{"status", "statusText", "httpVersion", "cookies", "headers", "content", "redirectURL", "headersSize", "bodySize"} {
		if _, ok := response[key]; !ok {
			t.Errorf("response.%s missing", key)
		}
	}
	for _, key := range []string{"size", "mimeType"} {
		if _, ok := content[key]; !ok {
			t.Errorf("content.%s missing", key)
		}
	}
	for _, key := range []string{"blocked", "dns", "connect", "send", "wait", "receive", "ssl"} {
		if _, ok := timings[key]; !ok {
			t.Errorf("timings.%s missing", key)
		}
	}
	if _, ok := root["cache"]; !ok {
		t.Errorf("entry.cache missing")
	}
}
