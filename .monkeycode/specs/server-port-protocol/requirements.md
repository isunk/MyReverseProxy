# Requirements Document

Feature Name: server-port-protocol
Updated: 2026-09-30

## Introduction

将 mrp 的接入模型改为 nginx 风格的多端口入口：每个 server（域名分组）声明接入协议 `protocol: http|https`，省略则自适应（按连接首字节识别 http/https），端口 `port` 可选，省略时按协议取默认端口（https 为 443、其余为 80）。监听端口完全由配置决定，`--port` 全局端口参数移除；协议嗅探仅保留在省略 `protocol` 的端口。同时修正 `upstream` 解析的两处易错点：省略 scheme 的 `host` / `host:port` 简写按 http 转发；本地目录识别改为分隔符规则，消除裸域名（如 `example.com`）被静默当成静态目录的问题。

## Glossary

- **server**: 配置中一个域名分组（domain + 可选 port/protocol + routes），是路由与监听的基本单位
- **入口端口**: server 实际绑定的 TCP 端口，取显式 `port`，省略时按协议取默认端口
- **默认端口**: 协议对应的约定端口，https 为 443、其余（http 与省略）为 80
- **协议 (protocol)**: 端口的接入协议，取值 `http` / `https`，省略则自适应（按连接首字节识别）
- **上游 (upstream)**: 路由转发目标，远程地址（完整 URL 或 `host[:port]` 简写）或本地静态目录
- **透传**: 域名与端口均未命中路由时按原始目标直接转发的现有行为
- **CONNECT 隧道**: 客户端以 CONNECT 方法建立的代理隧道，mrp 在隧道内按 SNI 现场签发证书并继续路由（现有 MITM 行为）
- **热加载**: 修改 `config.yaml` 后无需重启即生效的现有机制

## Requirements

### Requirement 1: server 端口与协议解析

**User Story:** 作为联调人员，我希望每个 server 用 protocol 声明接入协议并可选指定端口，省略端口时按协议走 80/443，以便用 nginx 风味的配置表达入口。

#### Acceptance Criteria

1. THE 代理服务 SHALL 为每个 server 解析出唯一接入协议：`protocol` 配置为 `http` 或 `https`，省略时按连接首字节自适应识别 http/https
2. WHEN server 配置了 `port`，THE 代理服务 SHALL 将该 server 绑定到该端口（整数 1-65535）
3. WHEN server 未配置 `port`，THE 代理服务 SHALL 按协议取默认端口：https 为 443、其余（http 与省略）为 80
4. IF `protocol` 取值不是 `http` 或 `https`，或 `port` 超出 1-65535，THE 代理服务 SHALL 拒绝加载配置并保留当前生效配置
5. IF 同一端口上多个 server 的协议不一致，THE 代理服务 SHALL 拒绝加载配置
6. THE 代理服务 SHALL 允许同一域名分别声明 http 与 https 两个端口条目
7. IF 同一端口上出现重复域名，THE 代理服务 SHALL 拒绝加载配置

### Requirement 2: 监听管理

**User Story:** 作为联调人员，我希望监听端口完全由配置决定并随热加载自动增减，以便无需重启即可调整入口。

#### Acceptance Criteria

1. THE 代理服务 SHALL 为配置中出现的每个不同端口建立一条监听，端口内按域名区分路由
2. WHEN 配置未包含任何 server，THE 代理服务 SHALL 保持无监听并输出提示日志，热加载新增 server 后自动开始监听
3. WHEN 热加载后的配置新增端口，THE 代理服务 SHALL 自动开始监听该端口
4. WHEN 热加载后的配置不再引用某端口，THE 代理服务 SHALL 关闭该端口监听并停止接受新连接，存量连接按自身生命周期自然结束
5. IF 配置解析或监听建立失败，THE 代理服务 SHALL 保留当前全部监听与路由表
6. WHEN 端口被占用或无权限绑定（如 Linux 非特权用户绑定 80），THE 代理服务 SHALL 拒绝本次加载并输出包含端口与原因的错误日志
7. WHEN 配置存在 https 端口但未提供 CA 证书，THE 代理服务 SHALL 在加载配置时报错并提示需要证书
8. THE 代理服务 SHALL 在 http 端口仅接受明文 HTTP（含 CONNECT 隧道请求），在 https 端口仅接受 TLS 连接并按 SNI 匹配域名、用 CA 现场签发证书；在省略 `protocol` 的自适应端口按连接首字节分别服务 HTTP 明文与 TLS 直连

### Requirement 3: 端口绑定的域名匹配

**User Story:** 作为联调人员，我希望域名匹配与到达端口绑定，以便同一域名可同时提供 http 与 https 服务且行为可预期。

#### Acceptance Criteria

1. WHEN 请求到达某端口，THE 代理服务 SHALL 仅在该端口分组的 server 集合内按域名与最长路径前缀匹配路由
2. WHEN 请求的域名在该端口无匹配 server，THE 代理服务 SHALL 透传原目标
3. THE 代理服务 SHALL 按 CONNECT 隧道建立所在端口分组匹配隧道内解密后的请求（现有隧道 MITM 行为保持）
4. THE 代理服务 SHALL 保持域名匹配大小写不敏感（SNI 与 Host 头），路由前缀按路径段边界最长匹配
5. WHEN 端口或协议配置变化，THE 代理服务 SHALL 判定配置指纹变化并重建处理器与监听分组

### Requirement 4: 启动参数与部署脚本

**User Story:** 作为联调人员，我希望端口模型收敛到配置一处，部署脚本与文档同步，以免残留失效的 --port 概念。

#### Acceptance Criteria

1. THE 代理服务 SHALL 移除 `--port` 启动参数，监听端口完全由配置决定
2. THE mrp.bat SHALL 将代理端口常量默认值改为 80，并注明其取值须与配置中某个监听端口一致
3. THE 默认配置模板 SHALL 说明 `port` 与 `protocol` 字段、默认端口规则及 upstream 简写
4. THE README SHALL 同步 servers 字段说明、示例、启动参数表，并标注升级行为变化（旧配置 server 无 port/protocol 时由全局端口变为自适应:80）
5. THE 代理服务 SHALL 保持旧配置文件可解析：无 `port`/`protocol` 字段的 server 按自适应:80 生效

### Requirement 5: upstream 简写与目录识别

**User Story:** 作为联调人员，我希望用更短的 upstream 写法表达常见目标，并让写错的 upstream 显式报错而非被静默当成目录。

#### Acceptance Criteria

1. THE 代理服务 SHALL 支持省略 scheme 的 upstream 简写：`host` 按 http、80 端口转发；`host:port` 按 http、指定端口转发
2. THE 代理服务 SHALL 继续支持 `http://host[:port][/path]` 与 `https://host[:port][/path]` 完整写法及路径前缀映射
3. WHEN upstream 含路径分隔符（`/` 或 `\`），THE 代理服务 SHALL 按本地静态目录处理（含 `./`、`../`、Windows 盘符路径）
4. WHEN upstream 为不含分隔符的裸词或 `host:port`（含 `[IPv6]:port`），THE 代理服务 SHALL 按远程上游解析
5. IF upstream 带非 http/https 的 scheme 前缀（如 `ftp://`），THE 代理服务 SHALL 拒绝加载并报不支持的 scheme
6. IF 简写 `host:port` 的端口超出 1-65535 或 host 为空，THE 代理服务 SHALL 拒绝加载并报明确错误
