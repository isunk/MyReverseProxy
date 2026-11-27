package capture

import (
	"encoding/json"
	"testing"
)

func markerEntry(marker string) Entry {
	return Entry{
		Request:  Request{Method: "GET"},
		Response: Response{Content: Content{Text: marker}},
	}
}

func TestSession_EntriesPreserveOrder(t *testing.T) {
	session := NewSession(3, 0)
	session.Add(markerEntry("a"))
	session.Add(markerEntry("b"))
	session.Add(markerEntry("c"))
	entries := session.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	for i, want := range []string{"a", "b", "c"} {
		if entries[i].Response.Content.Text != want {
			t.Fatalf("entries[%d] = %q, want %q", i, entries[i].Response.Content.Text, want)
		}
	}
}

func TestSession_EvictsOldestByCount(t *testing.T) {
	session := NewSession(3, 0)
	for _, marker := range []string{"a", "b", "c", "d", "e"} {
		session.Add(markerEntry(marker))
	}
	entries := session.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	for i, want := range []string{"c", "d", "e"} {
		if entries[i].Response.Content.Text != want {
			t.Fatalf("entries[%d] = %q, want %q", i, entries[i].Response.Content.Text, want)
		}
	}
}

func TestSession_EvictsByBytes(t *testing.T) {
	// 每条 body 6 字节，预算 10 字节，第二条加入后累计 12 字节触发回收最旧一条。
	session := NewSession(100, 10)
	session.Add(markerEntry("aaaaaa"))
	session.Add(markerEntry("bbbbbb"))
	entries := session.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Response.Content.Text != "bbbbbb" {
		t.Fatalf("surviving entry = %q, want bbbbbb", entries[0].Response.Content.Text)
	}
}

func TestSession_HARValid(t *testing.T) {
	session := NewSession(0, 0)
	session.Add(markerEntry("payload"))
	har := session.HAR()
	if har.Log.Version != "1.2" {
		t.Fatalf("version = %q, want 1.2", har.Log.Version)
	}
	if har.Log.Creator.Name != creatorName {
		t.Fatalf("creator = %q, want %q", har.Log.Creator.Name, creatorName)
	}
	if len(har.Log.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(har.Log.Entries))
	}
	// 空会话序列化应为合法 JSON，且 entries 非 nil。
	empty, err := json.Marshal(NewSession(0, 0).HAR())
	if err != nil {
		t.Fatalf("marshal empty HAR: %v", err)
	}
	if string(empty) != `{"log":{"version":"1.2","creator":{"name":"mrp","version":"1.0.0"},"pages":[],"entries":[]}}` {
		t.Fatalf("empty HAR marshal = %s", empty)
	}
}
