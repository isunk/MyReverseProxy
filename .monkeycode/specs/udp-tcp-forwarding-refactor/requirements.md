# Requirements Document

## Introduction

mrp 当前仅支持 TCP 单端口入口（HTTP/HTTPS 代理 + SOCKS5 CONNECT）。本需求集将扩展至 UDP 领域（SOCKS5 UDP ASSOCIATE），并在此过程中按设计模式重构代码以消除冗余。配置**仅支持 http(s) 协议**，不提供 TCP/UDP 端口转发。

## Glossary

- **SOCKS5 UDP ASSOCIATE**: SOCKS5 协议的 UDP 关联命令（0x03），客户端通过 TCP 控制通道协商后，在独立的 UDP 套接字上收发带 SOCKS5 头的 UDP 报文。
- **协议处理器 (Protocol Handler)**: 对一条入站连接的处理策略，按入口协议（HTTP、SOCKS5）选择。
- **统一监听模型**: 主端口监听共享统一的 Accept 循环、连接计数与优雅退出语义。

## Requirements

### Requirement 1: SOCKS5 UDP ASSOCIATE

**User Story:** AS SOCKS5 客户端用户, I want 通过 SOCKS5 代理收发 UDP 报文, so that DNS 查询等 UDP 流量也能经代理转发。

#### Acceptance Criteria

1. WHEN SOCKS5 客户端发送 UDP ASSOCIATE 命令（0x03），the system SHALL 在本地绑定一个 UDP 套接字并在 SOCKS5 回复中返回该套接字的绑定地址与端口。
2. WHEN UDP 报文到达 SOCKS5 UDP 中继套接字，the system SHALL 解析 SOCKS5 UDP 请求头（RSV/FRAG/ATYP/DST.ADDR/DST.PORT），将净荷转发到目标地址。
3. WHEN 目标返回 UDP 报文，the system SHALL 在净荷前补 SOCKS5 UDP 响应头并发回客户端源地址。
4. IF 分片标志（FRAG）非零，the system SHALL 丢弃该报文并跳过（不支持分片重组）。
5. WHILE TCP 控制连接保持打开，the system SHALL 维持 UDP 关联；TCP 控制连接关闭后，the system SHALL 关闭对应的 UDP 中继套接字并释放关联资源。
6. WHEN UDP 中继套接字无活跃流量达到空闲超时，the system SHALL 自动关闭该中继套接字。

### Requirement 2: 代码重构 — 消除冗余

**User Story:** AS 代码维护者, I want 重复的拨号-透传逻辑与连接服务模式抽取为共享组件, so that 新增协议入口时只需实现差异部分。

#### Acceptance Criteria

1. WHEN SOCKS5 CONNECT 与 HTTP CONNECT 隧道都需要"拨号目标 → 双向搬运字节"，the system SHALL 共用同一套 dial+relay 逻辑，差异仅在拨号参数与连接前后的协议握手。
2. WHEN SOCKS5 CONNECT 与 UDP ASSOCIATE 共用地址类型解析（IPv4/Domain/IPv6），the system SHALL 共用同一套 SOCKS5 地址解析函数。
3. WHEN 主端口监听需要 Accept 循环、退避重试与连接计数，the system SHALL 共用同一套 TCP Accept 服务模式。

### Requirement 3: 代码重构 — 设计模式优化

**User Story:** AS 架构师, I want 按设计模式组织连接处理与协议分发, so that 代码结构清晰且可扩展。

#### Acceptance Criteria

1. WHEN 不同入口协议（HTTP、SOCKS5）有各自的处理流程但共享连接生命周期管理，the system SHALL 使用策略模式将协议处理与连接管理解耦。
2. WHEN 配置构建涉及多种条目类型的校验与组装，the system SHALL 使用统一构建流程。
3. WHEN 新增协议处理器无需修改现有分发逻辑，the system SHALL 满足开闭原则。

### Requirement 4: 配置仅支持 HTTP/HTTPS

**User Story:** AS 配置编写者, I want 配置文件明确约束仅支持 http(s) 协议, so that 不支持的 TCP/UDP 转发不会被误配置。

#### Acceptance Criteria

1. WHEN 配置文件出现 `forwards` 段或 `servers[].protocol` 取非 `http`/`https` 值，the system SHALL 在配置加载阶段报错并保留旧配置生效。
2. WHEN `servers[].protocol` 省略或为 `http`/`https`，the system SHALL 正常按路由表处理。
3. WHEN 配置文件不含任何转发规则，the system SHALL 不启动额外监听器，行为与当前版本一致。
