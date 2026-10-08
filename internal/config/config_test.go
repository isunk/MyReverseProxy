package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mrp/internal/log"
	"mrp/internal/route"
	"mrp/internal/testutil"
)

func init() {
	log.SetOutput(io.Discard)
}

func TestLoadTable_Valid(t *testing.T) {
	path := testutil.ConfigFile(t, "r.yaml", `
servers:
  - domain: a.example.com
    routes:
      - prefix: /v1/
        upstream: http://up-a/v1/
      - prefix: /
        upstream: http://up-a
  - domain: b.example.com
    routes:
      - prefix: /
        upstream: https://up-b
`)
	table, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !table.Has("", 0, "a.example.com") || !table.Has("", 0, "b.example.com") {
		t.Fatal("want routes for a.example.com and b.example.com")
	}
	picked, ok := table.Pick("", 0, "a.example.com", "/v1/x")
	if !ok || picked.Prefix != "/v1/" {
		t.Fatalf("pick /v1/: got %+v ok=%v", picked, ok)
	}
	if _, ok := table.Pick("", 0, "b.example.com", "/"); !ok {
		t.Fatalf("pick b.example.com: not found")
	}
}

func TestLoadTable_Errors(t *testing.T) {
	cases := map[string]string{
		"empty_domain": "servers:\n  - domain: \"\"\n    routes: []",
		"dup_domain":   "servers:\n  - domain: a\n    routes: []\n  - domain: a\n    routes: []",
		"empty_prefix": "servers:\n  - domain: a\n    routes:\n      - prefix: \"\"\n        upstream: http://x",
		"dup_prefix":   "servers:\n  - domain: a\n    routes:\n      - prefix: /a\n        upstream: http://x\n      - prefix: /a\n        upstream: http://y",
		"bad_upstream": "servers:\n  - domain: a\n    routes:\n      - prefix: /\n        upstream: ftp://x",
		"bad_prefix":   "servers:\n  - domain: a\n    routes:\n      - prefix: v1\n        upstream: http://x",
		"bad_yaml":     "servers: [this is broken",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := testutil.ConfigFile(t, name+".yaml", content)
			if _, _, err := Load(path); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestLoadTable_LocalPathUpstream(t *testing.T) {
	cases := map[string]string{
		"relative": "./dist",
		"unix_abs": "/var/www",
		"win_back": `C:\Users\me\dist`,
		"win_fwd":  "D:/web/dist",
		"unc":      `\\server\share`,
	}
	for name, upstream := range cases {
		t.Run(name, func(t *testing.T) {
			content := "servers:\n  - domain: a.example.com\n    routes:\n      - prefix: /\n        upstream: " + upstream + "\n"
			path := testutil.ConfigFile(t, name+".yaml", content)
			table, _, err := Load(path)
			if err != nil {
				t.Fatalf("Load upstream=%q: unexpected error: %v", upstream, err)
			}
			picked, ok := table.Pick("", 0, "a.example.com", "/")
			if !ok {
				t.Fatalf("route not found for upstream %q", upstream)
			}
			if picked.Target.Root != upstream {
				t.Fatalf("target root: got %q want %q", picked.Target.Root, upstream)
			}
			if picked.Target.URL != nil {
				t.Fatalf("target url should be nil for local path %q", upstream)
			}
		})
	}
}

func TestLoadTable_NormalizesDomainCase(t *testing.T) {
	path := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: API.Example.COM\n    routes:\n      - prefix: /\n        upstream: http://up-a\n")
	table, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !table.Has("", 0, "api.example.com") {
		t.Fatal("domain should be normalized to lowercase")
	}
}

func TestLoadTable_TrimsWhitespace(t *testing.T) {
	path := testutil.ConfigFile(t, "r.yaml",
		"servers:\n  - domain: \" api.example.com \"\n    routes:\n      - prefix: \" /v1/ \"\n        upstream: \" http://up-a \"\n        host: \" up-a.example.com \"\n")
	table, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !table.Has("", 0, "api.example.com") {
		t.Fatal("domain should be trimmed")
	}
	picked, ok := table.Pick("", 0, "api.example.com", "/v1/x")
	if !ok || picked.Prefix != "/v1/" {
		t.Fatalf("prefix should be trimmed: got %+v ok=%v", picked, ok)
	}
	if picked.Host != "up-a.example.com" {
		t.Fatalf("host should be trimmed: got %q", picked.Host)
	}
	if picked.Target.URL.Host != "up-a" {
		t.Fatalf("upstream should be trimmed: got %q", picked.Target.URL.Host)
	}
}

func TestRouteTable_Fingerprint(t *testing.T) {
	base := "servers:\n  - domain: api.example.com\n    routes:\n      - prefix: /\n        upstream: http://up-a:8080\n"
	loaded := func(content string) *route.Table {
		table, _, err := Load(testutil.ConfigFile(t, "r.yaml", content))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return table
	}
	same := func(a, b *route.Table, what string) {
		if a.Fingerprint() != b.Fingerprint() {
			t.Fatalf("%s must not change the fingerprint: %q != %q", what, a.Fingerprint(), b.Fingerprint())
		}
	}
	different := func(a, b *route.Table, what string) {
		if a.Fingerprint() == b.Fingerprint() {
			t.Fatalf("%s must change the fingerprint", what)
		}
	}
	headerOnly := strings.Replace(base, "upstream: http://up-a:8080\n",
		"upstream: http://up-a:8080\n        headers:\n          response:\n            X-Test: \"1\"\n", 1)
	same(loaded(base), loaded(base), "identical config")
	different(loaded(base), loaded(strings.Replace(base, "up-a", "up-b", 1)), "upstream change")
	different(loaded(base), loaded(headerOnly), "header-only change")
	same(loaded(base), loaded(base+"nameservers:\n  - \"127.0.0.1:53\"\n"), "nameservers-only change")
}

func TestEnsureConfig_CreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := Ensure(path); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "servers:") {
		t.Fatalf("默认配置应包含 servers 字段，got %q", data)
	}
	if err := os.WriteFile(path, []byte("custom: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(path); err != nil {
		t.Fatalf("Ensure existing: %v", err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "custom: true\n" {
		t.Fatalf("已存在的文件被覆盖：%q", data)
	}
}

func TestDefaultConfig_Parses(t *testing.T) {
	table, nameservers, err := Load(testutil.ConfigFile(t, "default.yaml", defaultConfig))
	if err != nil {
		t.Fatalf("Load(defaultConfig): %v", err)
	}
	if table.Fingerprint() != "" {
		t.Fatalf("default table should be empty, got %q", table.Fingerprint())
	}
	if len(nameservers) != 0 {
		t.Fatalf("default nameservers = %v", nameservers)
	}
}

func TestDefaultConfig_DocumentsNameservers(t *testing.T) {
	for _, line := range []string{"# nameservers:", "#   - \"114.114.114.114\"", "nameservers: []"} {
		if !strings.Contains(defaultConfig, line) {
			t.Fatalf("defaultConfig missing %q", line)
		}
	}
}

func TestLoadTable_RejectsUnknownField(t *testing.T) {
	config := "servers: []\nnameserver: [\"114.114.114.114\"]\n"
	if _, _, err := Load(testutil.ConfigFile(t, "typo.yaml", config)); err == nil {
		t.Fatal("Load accepted unknown field nameserver")
	}
}

func TestLoadTable_EmptyConfigMessage(t *testing.T) {
	for name, content := range map[string]string{
		"empty":           "",
		"comment_only":    "# 只有注释\n",
		"whitespace_only": "   \n   \n",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := Load(testutil.ConfigFile(t, name+".yaml", content))
			if err == nil {
				t.Fatal("empty config must fail")
			}
			if !strings.Contains(err.Error(), "config file is empty") {
				t.Fatalf("error should mention empty config, got %q", err)
			}
		})
	}
}

func TestLoadTable_AcceptsUTF8BOM(t *testing.T) {
	config := "\ufeffservers: []\nnameservers: [\"114.114.114.114\"]\n"
	if _, _, err := Load(testutil.ConfigFile(t, "bom.yaml", config)); err != nil {
		t.Fatalf("Load(bom): %v", err)
	}
}
