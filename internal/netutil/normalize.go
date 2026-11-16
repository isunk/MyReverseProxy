package netutil

import (
	"strconv"
	"strings"
)

// NormalizeDomain 归一目标域名为小写并剥离 FQDN 尾点，使 DNS 绝对形式与配置视为同一域。
func NormalizeDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(domain), ".")
}

// ParsePort 解析端口字符串为 1-65535 的 int：strconv.Itoa 往返校验拒绝前导零、正负号、空白等非规范写法。
func ParsePort(port string) (int, bool) {
	number, err := strconv.Atoi(port)
	if err != nil || strconv.Itoa(number) != port || number < 1 || number > 65535 {
		return 0, false
	}
	return number, true
}

// ValidPortInt 判定端口号是否在 1-65535 范围内。
func ValidPortInt(port int) bool {
	return port >= 1 && port <= 65535
}
