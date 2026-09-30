# Requirements Document

Feature Name: server-port-protocol
Updated: 2026-09-30

## Introduction

为 mrp 的 `servers` 配置增加入口级自定义能力：每个 server（域名分组）可选声明监听端口 `port` 与接入协议 `protocol: http|https`，两者均省略时沿用现有全局 `--port` 与按连接嗅探的行为。同时修正 `upstream` 解析的两处易错点：省略 scheme 的 `host` / `host:port` 简写按 http 转发；本地目录识别改为分隔符规则，消除裸域名（如 `example.com`）被静默当成静态目录的问题。

## Glossary

- **全局端口**: 命令行 `--port` 指定的监听端口（默认 4000），未声明 `port` 的 server 绑定于此
- **自定义端口**: server 配置项 `port` 声明的监听端口，由代理服务按配置动态建立与回收监听
- **协议 (protocol)**: 端口的接入协议，取值 `http` / `https`；省略表示按连接首字节自动识别
- **自动嗅探**: 现有行为，按每条连接首字节是否为 TLS handshake（0x16）识别 HTTP/HTTPS
- **上游 (upstream)**: 路由转发目标，远程地址（完整 URL 或 `host[:port]` 简写）或本地静态目录
- **透传**: 域名与端口均未命中路由时按原始目标直接转发的现有行为
- **热加载**: 修改 `config.yaml` 后无需重启即生效的现有机制

## Requirements

### Requirement 1: server 入口端口

**User Story:** 作为联调人员，我希望为不同 server 指定各自的监听端口，以便在同一进程内按端口区分流量入口。

#### Acceptance Criteria

1. THE 代理服务 SHALL 支持 server 配置项 `port`（整数 1-65535），并把该 server 的域名路由绑定到该端口
2. WHEN server 未配置 `port`，THE 代理服务 SHALL 把该 server 绑定到 `--port` 指定的全局端口
3. THE 代理服务 SHALL 始终监听全局端口，即使没有 server 绑定于全局端口
4. WHEN 多个 server 声明相同自定义端口，THE 代理服务 SHALL 为该端口只建立一条监听，端口内按域名区分路由
5. IF `port` 取值超出 1-65535，THE 代理服务 SHALL 拒绝加载配置并保留当前生效配置
6. IF 自定义端口与全局端口相同，THE 代理服务 SHALL 拒绝加载配置并提示端口冲突

### Requirement 2: server 接入协议

**User Story:** 作为联调人员，我希望按 server 固定接入协议为 http 或 https，以便明文服务与 TLS 服务各自独占端口、行为可预期。

#### Acceptance Criteria

1. WHEN server 配置 `protocol: http`，THE 代理服务 SHALL 在该 server 所在端口仅接受明文 HTTP 请求
2. WHEN server 配置 `protocol: https`，THE 代理服务 SHALL 在该 server 所在端口仅接受 TLS 连接，并按 SNI 匹配域名、用 CA 现场签发证书
3. WHEN server 未配置 `protocol`，THE 代理服务 SHALL 在该端口按连接首字节自动识别 HTTP 与 TLS
4. IF `protocol` 取值不是 `http` 或 `https`，THE 代理服务 SHALL 拒绝加载配置并保留当前生效配置
5. IF 同一端口上多个 server 声明的 `protocol` 不一致（含一个声明、另一个省略），THE 代理服务 SHALL 拒绝加载配置
6. IF 绑定全局端口的 server 声明了 `protocol`，THE 代理服务 SHALL 拒绝加载配置并提示 protocol 仅支持自定义端口
7. IF 配置未提供 CA 证书且存在 `protocol: https` 的端口，THE 代理服务 SHALL 在加载配置时报错并提示需要证书；全局端口的自动嗅探模式保持现有运行期告警行为

### Requirement 3: 动态监听生命周期

**User Story:** 作为联调人员，我希望自定义端口随配置热加载自动增减，以便调整入口端口时无需重启进程。

#### Acceptance Criteria

1. WHEN 热加载后的配置新增自定义端口，THE 代理服务 SHALL 自动开始监听该端口
2. WHEN 热加载后的配置不再引用某自定义端口，THE 代理服务 SHALL 关闭该端口监听并停止接受新连接，存量连接按自身生命周期自然结束
3. IF 配置解析或监听建立失败，THE 代理服务 SHALL 保留当前全部监听与路由表
4. WHEN 自定义端口被其他进程占用，THE 代理服务 SHALL 拒绝本次加载并输出包含端口的错误日志
5. WHEN 自定义端口的配置内容未变化，THE 代理服务 SHALL 保持该监听不中断（热加载不拆存量连接）

### Requirement 4: 端口绑定的域名匹配

**User Story:** 作为联调人员，我希望域名匹配与声明端口绑定，以便同一域名在不同端口上的行为明确、可预期。

#### Acceptance Criteria

1. WHEN 请求到达某端口，THE 代理服务 SHALL 仅在绑定该端口的 server 集合内按域名与最长路径前缀匹配路由
2. WHEN 声明了自定义端口的域名请求到达其他端口，THE 代理服务 SHALL 按未命中路由透传原目标
3. WHEN 请求到达某端口但域名在该端口无匹配 server，THE 代理服务 SHALL 透传原目标
4. THE 代理服务 SHALL 保持域名匹配大小写不敏感（SNI 与 Host 头），路由前缀按路径段边界最长匹配
5. WHEN 端口或协议配置变化，THE 代理服务 SHALL 判定配置指纹变化并重建处理器与监听分组

### Requirement 5: upstream 简写与目录识别

**User Story:** 作为联调人员，我希望用更短的 upstream 写法表达常见目标，并让写错的 upstream 显式报错而非被静默当成目录。

#### Acceptance Criteria

1. THE 代理服务 SHALL 支持省略 scheme 的 upstream 简写：`host` 按 http、80 端口转发；`host:port` 按 http、指定端口转发
2. THE 代理服务 SHALL 继续支持 `http://host[:port][/path]` 与 `https://host[:port][/path]` 完整写法及路径前缀映射
3. WHEN upstream 含路径分隔符（`/` 或 `\`），THE 代理服务 SHALL 按本地静态目录处理（含 `./`、`../`、Windows 盘符路径）
4. WHEN upstream 为不含分隔符的裸词或 `host:port`（含 `[IPv6]:port`），THE 代理服务 SHALL 按远程上游解析
5. IF upstream 带非 http/https 的 scheme 前缀（如 `ftp://`），THE 代理服务 SHALL 拒绝加载并报不支持的 scheme
6. IF 简写 `host:port` 的端口超出 1-65535 或 host 为空，THE 代理服务 SHALL 拒绝加载并报明确错误

### Requirement 6: 配置模板与文档

**User Story:** 作为联调人员，我希望默认配置模板与 README 同步说明新字段，以便不查源码即可正确编写配置。

#### Acceptance Criteria

1. THE 代理服务 SHALL 在自动生成的默认配置模板中说明 `port` 与 `protocol` 字段及 upstream 简写规则
2. THE README SHALL 同步 servers 字段说明、新增字段示例与端口/协议行为
3. THE 代理服务 SHALL 保持旧配置文件（无 `port`/`protocol` 字段）完全兼容，行为与升级前一致
