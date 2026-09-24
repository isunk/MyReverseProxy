# Device Reverse Proxy (Go 单二进制反向代理)

Feature Name: device-reverse-proxy
Updated: 2026-09-24

## Description

Go 实现的反向代理服务。单二进制 + 类 nginx 配置，监听 HTTP/HTTPS 端口，按域名（SNI / Host）与路径前缀路由转发到不同上游。TLS 证书由使用者预先创建并配置路径，程序启动时加载。同一监听端口兼容 HTTP 代理协议（CONNECT），可直接作为显式代理使用。证书创建与设备导入步骤见仓库根目录 README.md。

## Architecture

```mermaid
graph TD
    A["客户端请求"] --> B["监听 80/443"]
    B --> C{"请求类型"}
    C --> D["HTTP 按 Host 头"]
    C --> E["TLS 按 SNI"]
    C --> F["CONNECT 代理协议"]
    E --> G["加载用户证书 TLS 终结"]
    F --> G
    D --> H["路由器 server 匹配 + location 最长前缀"]
    G --> H
    H --> I["httputil.ReverseProxy"]
    I --> J["上游服务器"]
    H --> K["无匹配 透传原始目标"]
    L["类 nginx 配置文件"] --> B
    L --> H
```

流量路径说明：

1. 客户端流量经设备侧 hosts / DNS 指向或显式代理设置到达监听端口（引导方式由使用者负责）
2. 依据 Host 头或 SNI 识别域名，匹配 server 块；块内按最长路径前缀匹配 location
3. ReverseProxy 完成转发与 Host/路径改写；无匹配域名直接透传原始目标
4. HTTPS 连接使用配置指定的证书完成 TLS 终结，再进入同一路由流程

## Components and Interfaces

```
device-proxy/
├── cmd/proxyd/main.go      # 入口与 flags
├── internal/config/        # 类 nginx 配置解析与热加载
├── internal/router/        # 域名/路径路由器
├── internal/server/        # 监听器：HTTP / HTTPS / CONNECT
└── internal/logging/       # 请求日志
```

### cmd/proxyd

```
proxyd --config /data/local/tmp/proxy.conf --log-level info
```

- SIGHUP 触发热加载（配置与证书同步重载）

### internal/config

自定义递归下降解析器，解析子集：`listen`、`tls_certificate`、`tls_certificate_key`、`server`、`location`、`proxy_pass`、`proxy_set_header`、`proxy_tls_verify`、注释。

```go
type Config struct {
    Listen  ListenConfig
    TLS     TLSConfig
    Servers []Server
}
type ListenConfig struct {
    HTTP  string // 默认 ":80"
    HTTPS string // 默认 ":443"
}
type TLSConfig struct {
    Certificate string
    PrivateKey  string
}
type Server struct {
    Domain string
    Routes []Route
}
type Route struct {
    Prefix             string
    Upstream           *url.URL
    HostOverride       string
    InsecureSkipVerify bool
}
```

配置示例：

```nginx
listen 80;
listen 443;
tls_certificate certs/server.crt;
tls_certificate_key certs/server.key;

server api.target-app.com {
    location /v1/ {
        proxy_pass https://our-server-a.com/v1/;
    }
    location / {
        proxy_pass http://192.168.1.50:8080;
        proxy_set_header Host our-server-b.com;
    }
}

server cdn.target-app.com {
    location / {
        proxy_pass https://our-server-b.com;
    }
}
```

### internal/router

- `Pick(domain, path) (*Route, bool)`：server 块域名精确匹配，location 最长前缀匹配
- SNI 识别：crypto/tls `GetConfigForClient` 读取 ClientHelloInfo.ServerName
- HTTP 识别：请求 Host 头 / CONNECT 目标
- 加载期构建 `map[domain][]Route`，热加载整体原子替换（atomic.Pointer）

### internal/server

- HTTP 监听：读取 Host 头进路由器
- HTTPS 监听：tls.Server + GetConfigForClient 完成 SNI 识别，证书来自配置加载的 tls.Certificate
- CONNECT：回 200 后接管裸流，按目标地址透传或经 SNI 路由（复用 HTTPS 流程）
- 转发统一走 httputil.ReverseProxy，Rewrite 函数处理 Host 改写与路径前缀映射，ErrorHandler 返回 502
- 热加载时原子替换 tls.Certificate

### internal/logging

- slog 结构化输出 stdout：domain、path、matched rule、upstream、status、latency
- 级别经 --log-level 与配置文件控制

## Data Models

- 路由表：`map[domain][]Route`，热加载原子替换
- TLS：启动时从配置路径加载 `tls.Certificate`，热加载同步替换

## Correctness Properties

1. location 匹配遵循最长前缀；等长前缀冲突时启动期报错
2. 加载的服务端证书 SAN 覆盖全部被服务域名（由使用者生成证书时保证，README 提供命令）
3. 热加载失败时旧配置继续生效；成功时路由表原子替换，存量连接不受影响
4. 无匹配域名透传语义与无代理直连等价

## Error Handling

| 场景 | 处理 |
|------|------|
| 上游连接失败/超时 | 客户端收到 502，日志记录上游地址与错误 |
| 监听端口被占用 | 启动失败并列出冲突端口 |
| 配置语法错误 | 保留旧配置，日志输出行号与原因 |
| 证书文件缺失或解析失败 | 启动失败，输出证书路径与原因 |
| 客户端未信任导入的 CA | 客户端证书报错；按 README 步骤导入 CA |

## Test Strategy

1. 单元测试：config 解析（合法/非法/冲突前缀样例）、最长前缀匹配、证书加载
2. 集成测试：httptest 模拟上游 + 测试内临时生成的证书 + 随机端口，覆盖 HTTP / HTTPS / CONNECT / 无匹配透传 / 502 / SIGHUP 热加载
3. 构建验证：Makefile 产出 android/arm64、ios/arm64、linux/arm64、windows/amd64 四种纯静态二进制并在 CI 校验

## References

[^1]: (Website) - httputil.ReverseProxy https://pkg.go.dev/net/http/httputil#ReverseProxy
[^2]: (Website) - crypto/tls ClientHelloInfo https://pkg.go.dev/crypto/tls#ClientHelloInfo
[^3]: (Website) - openssl x509 https://www.openssl.org/docs/manmaster/apps/openssl-x509.html
