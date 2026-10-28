package route

import (
	"net/url"
	"strings"
)

// buildOutPath 按斜杠边界把上游基路径与入站剩余路径拼成 (Path, RawPath)，剩余路径取原始编码形态避免 %2F 被重新转义。
func buildOutPath(inboundURL *url.URL, prefix string, base *url.URL) (string, string) {
	restPath := strings.TrimPrefix(inboundURL.Path, prefix)
	restRawPath := strings.TrimPrefix(inboundURL.EscapedPath(), prefix)
	baseSlash := strings.HasSuffix(base.Path, "/")
	restSlash := strings.HasPrefix(restPath, "/")
	path := singleJoiningSlash(base.Path, restPath)
	if restRawPath == "" {
		return path, ""
	}
	raw := base.EscapedPath()
	switch {
	case baseSlash && restSlash:
		raw = raw + restRawPath[1:]
	case !baseSlash && !restSlash:
		raw = raw + "/" + restRawPath
	default:
		raw = raw + restRawPath
	}
	if raw == (&url.URL{Path: path}).EscapedPath() {
		raw = ""
	}
	return path, raw
}

// singleJoiningSlash 拼接两段路径，避免出现双斜杠或丢斜杠。
func singleJoiningSlash(base, rest string) string {
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
