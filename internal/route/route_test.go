package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPick_LongestPrefix(t *testing.T) {
	table := &Table{byDomain: map[string][]*Entry{
		"a": {{routes: []*Route{
			{Prefix: "/"},
			{Prefix: "/v1/"},
			{Prefix: "/v1/users/"},
			{Prefix: "/api"},
		}}},
	}}
	cases := map[string]string{
		"/v1/users/1": "/v1/users/",
		"/v1/other":   "/v1/",
		"/other":      "/",
		"/api":        "/api",
		"/api/x":      "/api",
		"/api-v2":     "/",
	}
	for path, want := range cases {
		matched, ok := table.Pick("", 0, "a", path)
		if !ok || matched.Prefix != want {
			t.Fatalf("pick %s: want %s got %+v ok=%v", path, want, matched, ok)
		}
	}
	if _, ok := table.Pick("", 0, "other", "/"); ok {
		t.Fatal("unknown domain should not match")
	}
	blocked := &Table{byDomain: map[string][]*Entry{
		"b": {{Protocol: "https", Port: 443, routes: []*Route{{Prefix: "/"}}}},
	}}
	if _, ok := blocked.Pick("http", 80, "b", "/"); ok {
		t.Fatal("protocol and port mismatch should not match")
	}
	if got, ok := blocked.Pick("https", 443, "b", "/"); !ok || got.Prefix != "/" {
		t.Fatalf("exact protocol and port should match, got %v ok=%v", got, ok)
	}
}

func TestSingleJoiningSlash(t *testing.T) {
	cases := []struct {
		base, rest, want string
	}{
		{"", "", "/"},
		{"", "foo", "/foo"},
		{"", "/foo", "/foo"},
		{"/v1/", "users", "/v1/users"},
		{"/v1", "/users", "/v1/users"},
		{"/v1/", "/users", "/v1/users"},
		{"/v1", "", "/v1/"},
		{"/", "", "/"},
		{"/", "foo", "/foo"},
	}
	for _, tc := range cases {
		if got := singleJoiningSlash(tc.base, tc.rest); got != tc.want {
			t.Fatalf("singleJoiningSlash(%q, %q) = %q, want %q", tc.base, tc.rest, got, tc.want)
		}
	}
}

// TestRewriteRequest_PreservesEscapedPath 验证含编码字符（%2F、%20）的路径转发后编码语义原样保留，前缀按解码字节对齐剥离，前缀不匹配时整段原样转发。
func TestRewriteRequest_PreservesEscapedPath(t *testing.T) {
	cases := []struct {
		prefix      string
		inURL       string
		wantPath    string
		wantEscaped string
	}{
		{"/api", "/api/x", "/v1/x", "/v1/x"},
		{"/api", "/api/a%2Fb", "/v1/a/b", "/v1/a%2Fb"},
		{"/api/", "/api/a%2Fb", "/v1/a/b", "/v1/a%2Fb"},
		{"/api", "/unmatched/a%2Fb", "/v1/unmatched/a/b", "/v1/unmatched/a%2Fb"},
		{"/api", "/api/x%20y", "/v1/x y", "/v1/x%20y"},
	}
	for _, tc := range cases {
		target, err := ParseTarget("http://up-a/v1")
		if err != nil {
			t.Fatalf("ParseTarget: %v", err)
		}
		handler := (&Route{Prefix: tc.prefix, Target: target}).buildProxy(nil)
		inURL, err := url.Parse(tc.inURL)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", tc.inURL, err)
		}
		// 与 ReverseProxy 一致：In 为原始请求、Out 为其克隆，SetURL 只就地改写 Out
		in := &http.Request{URL: inURL, Header: http.Header{}}
		request := &httputil.ProxyRequest{In: in, Out: in.Clone(context.Background())}
		handler.Rewrite(request)
		if request.Out.URL.Path != tc.wantPath || request.Out.URL.EscapedPath() != tc.wantEscaped {
			t.Fatalf("rewrite %q prefix %q: path = %q, escaped = %q, want %q / %q",
				tc.inURL, tc.prefix, request.Out.URL.Path, request.Out.URL.EscapedPath(), tc.wantPath, tc.wantEscaped)
		}
	}
}

// TestOutboundURL 验证改写后发往上游的完整 URL：上游 scheme 与 host、前缀剥离与基路径拼接、查询串保留。路由日志与真实转发共用该实现。
func TestOutboundURL(t *testing.T) {
	cases := []struct {
		prefix, upstream, inbound string
		want                      string
	}{
		{"/api", "http://up-a/v1", "http://a.local/api/users", "http://up-a/v1/users"},
		{"/api", "http://up-a/v1/", "http://a.local/api/users", "http://up-a/v1/users"},
		{"/api", "http://up-a:8080", "http://a.local/api", "http://up-a:8080/"},
		{"/api/v1", "http://up-a/v1", "http://a.local/api-v2/x", "http://up-a/v1/api-v2/x"},
		{"/api", "http://up-a/v1", "http://a.local/api/a%2Fb", "http://up-a/v1/a%2Fb"},
		{"/api", "http://up-a/v1?q=1", "http://a.local/api/x?a=1", "http://up-a/v1/x?q=1&a=1"},
	}
	for _, tc := range cases {
		target, err := ParseTarget(tc.upstream)
		if err != nil {
			t.Fatalf("ParseTarget(%q): %v", tc.upstream, err)
		}
		inbound, err := url.Parse(tc.inbound)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", tc.inbound, err)
		}
		r := &Route{Prefix: tc.prefix, Target: target}
		if got := r.OutboundURL(inbound, inbound.RawQuery).String(); got != tc.want {
			t.Fatalf("OutboundURL(%q, %q) = %q, want %q", tc.prefix, tc.inbound, got, tc.want)
		}
	}
}

func TestParseTarget(t *testing.T) {
	cases := map[string]struct {
		scheme string
		host   string
		root   string
	}{
		"http://up-a":      {scheme: "http", host: "up-a"},
		"https://up-a":     {scheme: "https", host: "up-a"},
		"up-a":             {scheme: "http", host: "up-a"},
		"up-a:8080":        {scheme: "http", host: "up-a:8080"},
		"[::1]:8080":       {scheme: "http", host: "[::1]:8080"},
		".":                {root: "."},
		"..":               {root: ".."},
		"./":               {root: "./"},
		"../":              {root: "../"},
		"./dist":           {root: "./dist"},
		"/var/www":         {root: "/var/www"},
		`C:\Users\me\dist`: {root: `C:\Users\me\dist`},
		"D:/web/dist":      {root: "D:/web/dist"},
		`\\server\share`:   {root: `\\server\share`},
	}
	for upstream, want := range cases {
		target, err := ParseTarget(upstream)
		if err != nil {
			t.Fatalf("ParseTarget(%q): %v", upstream, err)
		}
		if target.Root != want.root {
			t.Fatalf("ParseTarget(%q).Root = %q, want %q", upstream, target.Root, want.root)
		}
		if want.scheme == "" {
			if target.URL != nil {
				t.Fatalf("ParseTarget(%q).URL = %v, want nil", upstream, target.URL)
			}
			continue
		}
		if target.URL == nil || target.URL.Scheme != want.scheme || target.URL.Host != want.host {
			t.Fatalf("ParseTarget(%q).URL = %v, want scheme=%q host=%q", upstream, target.URL, want.scheme, want.host)
		}
	}
	for _, upstream := range []string{"ftp://x", "http://", "up-a:0", "up-a:bad", "::1"} {
		if _, err := ParseTarget(upstream); err == nil {
			t.Fatalf("ParseTarget(%q) accepted", upstream)
		}
	}
}

func TestTarget_SummaryPrecomputed(t *testing.T) {
	for upstream, want := range map[string]string{
		"http://up-a:8080": "http://up-a:8080",
		"up-a:8080":        "http://up-a:8080",
		"./dist":           "./dist",
		".":                ".",
	} {
		target, err := ParseTarget(upstream)
		if err != nil {
			t.Fatalf("ParseTarget(%q): %v", upstream, err)
		}
		if target.Summary != want {
			t.Fatalf("ParseTarget(%q).Summary = %q, want %q", upstream, target.Summary, want)
		}
	}
}

func TestStaticHandler_PathTraversal(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("home"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := newStaticHandler(root, "/", nil)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.URL.Path = "/../secret.txt"
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("path traversal: want 404, got %d body=%q", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "outside-secret") {
		t.Fatalf("path traversal: leaked outside file content: %q", recorder.Body.String())
	}
}

func TestStaticHandler_ResponseHeaders(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("home"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := newStaticHandler(root, "/", map[string]string{"Access-Control-Allow-Origin": "*"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("response header not applied: got %q", got)
	}
}

// "." 与 "./" 指向同一目录，必须都按本地目录托管并服务出同样的文件
func TestStaticHandler_DotRootEquivalent(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "index.html"), []byte("home"), 0o644); err != nil {
		t.Fatal(err)
	}
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	for _, root := range []string{".", "./"} {
		handler := newStaticHandler(root, "/", nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("root %q: code=%d, want %d", root, recorder.Code, http.StatusOK)
		}
		if body := recorder.Body.String(); body != "home" {
			t.Fatalf("root %q: body=%q, want %q", root, body, "home")
		}
	}
}

func TestStatusHandler_BareCode(t *testing.T) {
	handler := newStatusHandler(http.StatusNoContent, "", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("code=%d, want %d", recorder.Code, http.StatusNoContent)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("body=%q, want empty", recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "" {
		t.Fatalf("Content-Type=%q, want unset", recorder.Header().Get("Content-Type"))
	}
}

func TestStatusHandler_WithBody(t *testing.T) {
	handler := newStatusHandler(http.StatusOK, "OK", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("code=%d, want %d", recorder.Code, http.StatusOK)
	}
	if recorder.Body.String() != "OK" {
		t.Fatalf("body=%q, want %q", recorder.Body.String(), "OK")
	}
	if ct := recorder.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type=%q, want %q", ct, "text/plain; charset=utf-8")
	}
	if cl := recorder.Header().Get("Content-Length"); cl != "2" {
		t.Fatalf("Content-Length=%q, want %q", cl, "2")
	}
}

func TestStatusHandler_ResponseHeaders(t *testing.T) {
	handler := newStatusHandler(http.StatusMovedPermanently, "", map[string]string{"Location": "https://example.com/"})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusMovedPermanently {
		t.Fatalf("code=%d, want %d", recorder.Code, http.StatusMovedPermanently)
	}
	if loc := recorder.Header().Get("Location"); loc != "https://example.com/" {
		t.Fatalf("Location=%q, want %q", loc, "https://example.com/")
	}
}

func TestStatusHandler_HeadSkipsBody(t *testing.T) {
	handler := newStatusHandler(http.StatusOK, "body", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("code=%d, want %d", recorder.Code, http.StatusOK)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("body=%q, want empty for HEAD", recorder.Body.String())
	}
	if cl := recorder.Header().Get("Content-Length"); cl != "4" {
		t.Fatalf("Content-Length=%q, want %q", cl, "4")
	}
}
