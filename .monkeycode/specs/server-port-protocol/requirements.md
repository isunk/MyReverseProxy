# Requirements Document

Feature Name: server-port-protocol
Updated: 2026-09-30

## Introduction

mrp 收敛为单端口代理模型：只监听一个端口（`--port`，默认 8000），配置里的 `protocol` 与 `port` 不再决定监听，只作为虚拟服务器的匹配条件——决定哪个请求归属哪个 server。协议与端口从请求内容推导：明文连接取 Host 头与 URL，CONNECT 按目标主机与端口（视为 https）。未命中路由的域名透传原目标，未配置的 CONNECT 目标走透传隧道。同时保留 `upstream` 简写与本地目录识别规则。

## Glossary

- **虚拟服务器 (server)**: 配置中一个域名分组（domain + 可选 protocol/port + routes），是路由匹配的基本单位
- **匹配协议 (protocol)**: server 声明的请求协议 `http` / `https`，省略匹配任意协议
- **匹配端口 (port)**: server 声明的请求目标端口，省略匹配任意端口
- **请求协议**: 由请求内容推导的协议——明文连接按 URL scheme 或连接是否 TLS，CONNECT 视为 https
- **上游 (upstream)**: 路由转发目标，远程地址（完整 URL 或 `host[:port]` 简写）或本地静态目录
- **透传**: 域名与协议/端口均未命中路由时按原始目标直接转发的现有行为
- **CONNECT 隧道**: 客户端以 CONNECT 方法建立的代理隧道；命中配置的域名时 mrp 在隧道内现场签发证书并解密路由，未命中时只搬运字节
- **热加载**: 修改 `config.yaml` 后无需重启即生效的现有机制

## Requirements

### Requirement 1: 虚拟服务器匹配条件解析

**User Story:** 作为联调人员，我希望用 protocol 与 port 表达"哪类请求走哪组路由"，以便同一域名可以区分明文与加密流量而不新增端口。

#### Acceptance Criteria

1. THE 代理服务 SHALL 为每个 server 解析出匹配协议与匹配端口：`protocol` 取 `http` / `https`，省略匹配任意协议；`port` 取整数 1-65535，省略匹配任意端口
2. IF `protocol` 取值不是 `http` 或 `https`，或显式 `port` 超出 1-65535，THE 代理服务 SHALL 拒绝加载配置并保留当前生效配置
3. IF 同一 `(domain, protocol, port)` 组合重复声明，THE 代理服务 SHALL 拒绝加载配置
4. THE 代理服务 SHALL 允许同一域名声明多个 server 分别匹配不同协议或端口
5. THE 代理服务 SHALL 保持旧配置文件可解析：无 `protocol` / `port` 字段的 server 匹配任意协议与端口

### Requirement 2: 单端口监听

**User Story:** 作为联调人员，我希望代理端口只由一处参数决定，以便部署时与设备代理设置一一对应。

#### Acceptance Criteria

1. THE 代理服务 SHALL 仅在 `--port` 指定的端口建立一条监听，默认值为 80，取值须在 1-65535
2. IF `--port` 超出 1-65535 或无法绑定（端口被占用、无权限），THE 代理服务 SHALL 终止启动并输出包含端口与原因的错误日志
3. THE 代理服务 SHALL 在该端口始终接受明文 TCP 连接，HTTP 请求与 CONNECT 隧道按请求内容区分
4. THE 代理服务 SHALL 不在热加载时增减监听；配置热加载只替换路由表
5. THE mrp.bat SHALL 使用 80 作为代理端口常量，与该默认值一致

### Requirement 3: 协议与端口绑定的域名匹配

**User Story:** 作为联调人员，我希望请求按协议与目标端口筛选虚拟服务器，以便同一域名下明文与加密流量走不同路由。

#### Acceptance Criteria

1. WHEN 收到非 CONNECT 请求，THE 代理服务 SHALL 取 URL scheme 与目标端口作为请求协议与端口；省略端口时按协议补默认端口（https 443、其余 80）
2. WHEN 请求经 CONNECT 进入隧道内部，THE 代理服务 SHALL 按 https 与该域名匹配路由
3. WHEN 请求到达某域名，THE 代理服务 SHALL 仅在该域名下协议与端口均匹配的 server 集合内按最长路径前缀匹配路由
4. WHEN 请求的域名或协议端口无匹配 server，THE 代理服务 SHALL 透传原目标
5. THE 代理服务 SHALL 保持域名匹配大小写不敏感，路由前缀按路径段边界最长匹配
6. WHEN 协议或端口配置变化，THE 代理服务 SHALL 判定配置指纹变化并重建处理器

### Requirement 4: CONNECT 隧道与 MITM 分派

**User Story:** 作为联调人员，我希望命中配置的域名被解密路由、其余域名保持可用，以便在未覆盖的流量上不产生故障。

#### Acceptance Criteria

1. WHEN CONNECT 目标的域名在该协议端口下有匹配路由，THE 代理服务 SHALL 返回 200 并在隧道内按 SNI 现场签发证书完成 MITM，解密后的请求按域名与路径前缀路由
2. WHEN CONNECT 目标的域名无匹配路由，THE 代理服务 SHALL 返回 200 并只搬运字节透传到原目标
3. IF CONNECT 目标有匹配路由但未配置 CA 证书，THE 代理服务 SHALL 返回 502 并输出警告日志，不得透传解密流量
4. IF 上游不可达，THE 代理服务 SHALL 返回 502，不向客户端透出传输层错误细节
5. THE 代理服务 SHALL 在 CONNECT 与后续数据同批到达时保留已读到的字节，避免丢失

### Requirement 5: upstream 简写与目录识别

**User Story:** 作为联调人员，我希望用更短的 upstream 写法表达常见目标，并让写错的 upstream 显式报错而非被静默当成目录。

#### Acceptance Criteria

1. THE 代理服务 SHALL 支持省略 scheme 的 upstream 简写：`host` 按 http、80 端口转发；`host:port` 按 http、指定端口转发
2. THE 代理服务 SHALL 继续支持 `http://host[:port][/path]` 与 `https://host[:port][/path]` 完整写法及路径前缀映射
3. WHEN upstream 含路径分隔符（`/` 或 `\`），THE 代理服务 SHALL 按本地静态目录处理（含 `./`、`../`、Windows 盘符路径）
4. WHEN upstream 为不含分隔符的裸词或 `host:port`（含 `[IPv6]:port`），THE 代理服务 SHALL 按远程上游解析
5. IF upstream 带非 http/https 的 scheme 前缀（如 `ftp://`），THE 代理服务 SHALL 拒绝加载并报不支持的 scheme
6. IF 简写 `host:port` 的端口超出 1-65535 或 host 为空，THE 代理服务 SHALL 拒绝加载并报明确错误
