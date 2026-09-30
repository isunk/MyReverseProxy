package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTransport_CachesTLSSessions(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, "ok")
	}))
	t.Cleanup(upstream.Close)
	transport := NewTransport(func(ctx context.Context, network, address string) (net.Conn, error) {
		return net.Dial(network, address)
	})
	transport.MaxIdleConns = 0
	transport.MaxIdleConnsPerHost = 0
	transport.DisableKeepAlives = true
	requested := upstream.URL + "/"
	// NewSessionTicket 是 TLS 1.3 握手后消息，连接立即关闭时可能来不及送达，故多次尝试
	var resumed bool
	for i := range 3 {
		httpReq, err := http.NewRequest(http.MethodGet, requested, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := transport.RoundTrip(httpReq)
		if err != nil {
			t.Fatalf("roundtrip %d: %v", i+1, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.TLS == nil {
			t.Fatalf("roundtrip %d: missing tls state", i+1)
		}
		if i == 0 && resp.TLS.DidResume {
			t.Fatal("first upstream connection resumed unexpectedly")
		}
		resumed = resumed || resp.TLS.DidResume
	}
	if !resumed {
		t.Fatal("no upstream connection resumed the tls session")
	}
}
