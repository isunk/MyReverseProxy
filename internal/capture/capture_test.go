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

func TestRecoverPrevious_IncompleteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "capture.har")
	// 模拟崩溃：写入头部和一条 entry 但不闭合
	body := header + `{"startedDateTime":"t","request":{"method":"GET","url":"http://a/"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	recovered := RecoverPrevious(path)
	if recovered == "" {
		t.Fatal("RecoverPrevious returned empty for incomplete file")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("original file should be renamed away, stat err=%v", err)
	}
	data, err := os.ReadFile(recovered)
	if err != nil {
		t.Fatalf("ReadFile recovered: %v", err)
	}
	var har Har
	if err := json.Unmarshal(data, &har); err != nil {
		t.Fatalf("recovered file should be valid JSON: %v", err)
	}
	if len(har.Log.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(har.Log.Entries))
	}
}

func TestRecoverPrevious_CompleteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "capture.har")
	if err := os.WriteFile(path, []byte(header+"]}}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if recovered := RecoverPrevious(path); recovered != "" {
		t.Fatalf("RecoverPrevious should return empty for complete file, got %s", recovered)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("complete file should remain: %v", err)
	}
}

func TestRecoverPrevious_NoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.har")
	if recovered := RecoverPrevious(path); recovered != "" {
		t.Fatalf("RecoverPrevious should return empty for nonexistent file, got %s", recovered)
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
