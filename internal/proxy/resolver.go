package proxy

import (
	"context"
	"net"
)

// resolver 抽象 proxy 所需的上游解析与拨号能力，由 dns.Resolver 提供；依赖接口而非具体类型，便于测试注入替身与未来替换解析实现。
type resolver interface {
	Update([]string) (bool, error)
	Resolve(context.Context, string, int) ([]string, error)
	DialContext(context.Context, string, string) (net.Conn, error)
	ClearCache()
}
