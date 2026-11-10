package route

import (
	"net/url"
	"strings"
)

// buildOutPath 按斜杠边界把上游基路径与入站剩余路径拼成 (Path, RawPath)，剩余路径取原始编码形态避免 %2F 被重新转义。
func buildOutPath(inboundURL *url.URL, prefix string, base *url.URL) (string, string) {
	restPath := strings.TrimPrefix(inboundURL.Path, prefix)
	escaped := inboundURL.EscapedPath()
	restRawPath := strings.TrimPrefix(escaped, prefix)
	path := singleJoiningSlash(base.Path, restPath)
	// 解码侧命中而编码侧剥不掉（prefix 含需编码字符）时 restRawPath 与 restPath 错位，放弃 RawPath 改按解码 path 重编码，保证前缀剥离正确；前缀整体不匹配时两侧一致保留原样转发
	if strings.HasPrefix(inboundURL.Path, prefix) && !strings.HasPrefix(escaped, prefix) {
		return path, ""
	}
	raw := singleJoiningSlash(base.EscapedPath(), restRawPath)
	if raw == (&url.URL{Path: path}).EscapedPath() {
		raw = ""
	}
	return path, raw
}

// singleJoiningSlash 拼接两段路径，避免出现双斜杠或丢斜杠；rest 为空（路径完整命中前缀）时保留 base 原样，不额外补斜杠。
func singleJoiningSlash(base, rest string) string {
	if rest == "" {
		if base == "" {
			return "/"
		}
		return base
	}
	baseSlash := strings.HasSuffix(base, "/")
	restSlash := strings.HasPrefix(rest, "/")
	switch {
	case baseSlash && restSlash:
		return base + rest[1:]
	case !baseSlash && !restSlash:
		return base + "/" + rest
	}
	return base + rest
}
