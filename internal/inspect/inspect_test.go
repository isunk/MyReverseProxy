package inspect

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mrp/internal/capture"
)

func TestServeHar_ReturnsCurrentSession(t *testing.T) {
	session := capture.NewSession(capture.MaxEntries, capture.MaxBytes)
	session.Add(capture.Entry{
		Request:  capture.Request{Method: http.MethodGet, URL: "http://api.example.com/hello"},
		Response: capture.Response{Status: http.StatusOK},
	})
	server := New(session, "127.0.0.1:0")
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content-type = %q", got)
	}
	var har capture.Har
	if err := json.Unmarshal(response.Body.Bytes(), &har); err != nil {
		t.Fatalf("unmarshal HAR: %v", err)
	}
	if len(har.Log.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(har.Log.Entries))
	}
	if har.Log.Entries[0].Request.URL != "http://api.example.com/hello" {
		t.Fatalf("url = %q", har.Log.Entries[0].Request.URL)
	}
}

func TestServeHar_RejectsNonGet(t *testing.T) {
	server := New(capture.NewSession(0, 0), "127.0.0.1:0")
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.Code)
	}
}

func TestServeHar_HeadNoBody(t *testing.T) {
	server := New(capture.NewSession(0, 0), "127.0.0.1:0")
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodHead, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if response.Body.Len() != 0 {
		t.Fatalf("HEAD body = %d bytes, want 0", response.Body.Len())
	}
}
