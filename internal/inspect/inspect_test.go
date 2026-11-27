package inspect

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"mrp/internal/capture"
)

func TestServer_VersionAndList(t *testing.T) {
	server := New(capture.NewSession(0, 0), "127.0.0.1:0")
	ts := httptest.NewServer(server.server.Handler)
	defer ts.Close()

	response, err := http.Get(ts.URL + "/json/version")
	if err != nil {
		t.Fatalf("get /json/version: %v", err)
	}
	defer response.Body.Close()
	var version map[string]any
	if err := json.NewDecoder(response.Body).Decode(&version); err != nil {
		t.Fatalf("decode version: %v", err)
	}
	if version["Browser"] != "mrp/1.0.0" {
		t.Fatalf("Browser = %v, want mrp/1.0.0", version["Browser"])
	}
	if version["Protocol-Version"] != "1.3" {
		t.Fatalf("Protocol-Version = %v, want 1.3", version["Protocol-Version"])
	}

	response, err = http.Get(ts.URL + "/json/list")
	if err != nil {
		t.Fatalf("get /json/list: %v", err)
	}
	defer response.Body.Close()
	var list []map[string]any
	if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("targets = %d, want 1", len(list))
	}
	if list[0]["id"] != pageID {
		t.Fatalf("id = %v, want %s", list[0]["id"], pageID)
	}
	if list[0]["type"] != "page" {
		t.Fatalf("type = %v, want page", list[0]["type"])
	}
	wsURL, _ := list[0]["webSocketDebuggerUrl"].(string)
	if want := wsTarget(strings.TrimPrefix(ts.URL, "http://")); wsURL != want {
		t.Fatalf("webSocketDebuggerUrl = %q, want %q", wsURL, want)
	}
}

func TestServer_NetworkEnableDumpsHistory(t *testing.T) {
	session := capture.NewSession(0, 0)
	session.Add(capture.Entry{
		StartedDateTime: time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		Request:         capture.Request{Method: http.MethodGet, URL: "http://api.example.com/hello"},
		Response: capture.Response{
			Status:     http.StatusOK,
			StatusText: "OK",
			Content:    capture.Content{MimeType: "application/json", Text: "body"},
		},
	})
	server := New(session, "127.0.0.1:0")
	ts := httptest.NewServer(server.server.Handler)
	defer ts.Close()
	connection, _, err := websocket.DefaultDialer.Dial(wsTarget(strings.TrimPrefix(ts.URL, "http://")), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()

	if err := connection.WriteJSON(map[string]any{"id": 1, "method": "Network.enable", "params": map[string]any{}}); err != nil {
		t.Fatalf("send Network.enable: %v", err)
	}
	got := waitForNetworkEvents(t, connection)
	for _, method := range []string{"Network.requestWillBeSent", "Network.responseReceived", "Network.loadingFinished"} {
		if !got[method] {
			t.Fatalf("missing event %s; got %v", method, got)
		}
	}
}

func TestServer_GetResponseBody(t *testing.T) {
	session := capture.NewSession(0, 0)
	session.Add(capture.Entry{
		StartedDateTime: time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		Request:         capture.Request{Method: http.MethodGet, URL: "http://api.example.com/hello"},
		Response: capture.Response{
			Status:  http.StatusOK,
			Content: capture.Content{Text: "payload"},
		},
	})
	server := New(session, "127.0.0.1:0")
	ts := httptest.NewServer(server.server.Handler)
	defer ts.Close()
	connection, _, err := websocket.DefaultDialer.Dial(wsTarget(strings.TrimPrefix(ts.URL, "http://")), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()

	if err := connection.WriteJSON(map[string]any{"id": 2, "method": "Network.getResponseBody", "params": map[string]any{"requestId": "1"}}); err != nil {
		t.Fatalf("send getResponseBody: %v", err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, message, err := connection.ReadMessage()
	if err != nil {
		t.Fatalf("read getResponseBody result: %v", err)
	}
	var result struct {
		ID     int `json:"id"`
		Result struct {
			Body          string `json:"body"`
			Base64Encoded bool   `json:"base64Encoded"`
		} `json:"result"`
	}
	if err := json.Unmarshal(message, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Result.Body != "payload" {
		t.Fatalf("body = %q, want payload", result.Result.Body)
	}
	if result.Result.Base64Encoded {
		t.Fatal("base64Encoded = true, want false for text body")
	}
}

func TestServer_TargetInfoAndFrameTree(t *testing.T) {
	server := New(capture.NewSession(0, 0), "127.0.0.1:0")
	ts := httptest.NewServer(server.server.Handler)
	defer ts.Close()
	connection, _, err := websocket.DefaultDialer.Dial(wsTarget(strings.TrimPrefix(ts.URL, "http://")), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()

	if err := connection.WriteJSON(map[string]any{"id": 3, "method": "Target.getTargetInfo", "params": map[string]any{}}); err != nil {
		t.Fatalf("send Target.getTargetInfo: %v", err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, message, err := connection.ReadMessage()
	if err != nil {
		t.Fatalf("read targetInfo: %v", err)
	}
	var info struct {
		Result struct {
			TargetInfo struct {
				Type string `json:"type"`
			} `json:"targetInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(message, &info); err != nil {
		t.Fatalf("decode targetInfo: %v", err)
	}
	if info.Result.TargetInfo.Type != "page" {
		t.Fatalf("targetInfo.type = %q, want page", info.Result.TargetInfo.Type)
	}
}

func TestServer_BroadcastsNewRecords(t *testing.T) {
	session := capture.NewSession(0, 0)
	server := New(session, "127.0.0.1:0")
	if err := server.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer server.Close()

	connection, _, err := websocket.DefaultDialer.Dial(wsTarget(server.Addr()), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()
	if err := connection.WriteJSON(map[string]any{"id": 1, "method": "Network.enable", "params": map[string]any{}}); err != nil {
		t.Fatalf("send Network.enable: %v", err)
	}
	session.Add(capture.Entry{
		StartedDateTime: time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		Request:         capture.Request{Method: http.MethodGet, URL: "http://api.example.com/late"},
		Response:        capture.Response{Status: http.StatusOK, Content: capture.Content{Text: "late"}},
	})
	waitForNetworkEvents(t, connection)
}

func wsTarget(host string) string {
	return "ws://" + host + "/devtools/page/" + pageID
}

func waitForNetworkEvents(t *testing.T, connection *websocket.Conn) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	accumulated := []string{}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, message, err := connection.ReadMessage()
		if err != nil {
			t.Fatalf("read events: %v; got %v", err, accumulated)
		}
		var event map[string]any
		if err := json.Unmarshal(message, &event); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		method, _ := event["method"].(string)
		switch method {
		case "Network.requestWillBeSent", "Network.responseReceived", "Network.loadingFinished":
			got[method] = true
			accumulated = append(accumulated, method)
		}
		if got["Network.requestWillBeSent"] && got["Network.responseReceived"] && got["Network.loadingFinished"] {
			return got
		}
	}
}
