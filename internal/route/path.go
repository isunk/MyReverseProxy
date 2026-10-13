package route

import (
	"net/url"
	"strings"
)

// trimEncodedPrefix 在原始编码路径上剥掉指定字节数的解码前缀：EscapedPath 的
// 每一段（%XX 或单字节）恰好解码为 1 字节，与 Path 字节序列一一对应，
// 按字节数逐段消费即可，避免把 %2F 重新转义成 %252F。
func trimEncodedPrefix(escaped string, skip int) string {
	for ; skip > 0 && escaped != ""; skip-- {
		if escaped[0] == '%' && len(escaped) >= 3 {
			escaped = escaped[3:]
			continue
		}
		escaped = escaped[1:]
	}
	return escaped
}

// joinOutPath 按斜杠边界拼接上游基路径与入站剩余路径，语义与 httputil 内部
// joinURLPath 一致：Path 与 RawPath（完整路径编码）按同一斜杠规则同步拼接，
// RawPath 的 base 段取 base 的完整转义、rest 段取传入的编码剩余路径；
// 结果无编码差异时 RawPath 置空，回归默认转义。
func joinOutPath(base *url.URL, restPath, restRawPath string) (string, string) {
	baseRaw := base.EscapedPath()
	restRaw := restRawPath
	if restRaw == "" {
		restRaw = (&url.URL{Path: restPath}).EscapedPath()
	}
	baseSlash := strings.HasSuffix(base.Path, "/")
	restSlash := strings.HasPrefix(restPath, "/")
	path := singleJoiningSlash(base.Path, restPath)
	raw := baseRaw + restRaw
	switch {
	case baseSlash && restSlash:
		raw = baseRaw + restRaw[1:]
	case !baseSlash && !restSlash:
		raw = baseRaw + "/" + restRaw
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
