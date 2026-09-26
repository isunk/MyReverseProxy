# Requirements Document

Feature Name: mrp
Updated: 2026-09-24

## Introduction

我的反向代理（My Reverse Proxy，缩写 mrp）：基于 Go 实现的单文件反向代理服务。直接运行后监听单一端口，按连接首字节自动识别 HTTP / TLS，依据 YAML 路由配置文件，按请求的域名（SNI / Host）与路径前缀匹配自定义规则，将请求转发到不同的服务器。配置文件只承载路由规则，监听地址与 TLS 证书路径通过命令行参数指定。TLS 证书由使用者预先创建，程序仅加载使用，证书的创建与导入设备的步骤在 README.md 中以命令行方式给出。

## Glossary

- **代理服务 / mrp**: 本需求要开发的 Go 单文件程序，构建产物为静态单二进制 `mrp`
- **目标域名**: 需要被劫持转发的域名
- **上游 (Upstream)**: 路由配置中 proxy_pass 指定的目标服务器
- **TLS 证书文件**: 使用者通过 openssl 预先生成的证书与私钥文件（含 CA 证书与覆盖目标域名的服务端证书）

## Requirements

### Requirement 1: 启动与配置

**User Story:** 作为联调人员，我希望一个静态二进制加一份配置文件就能跑起来，并支持改配置不重启，以便快速部署与调整路由。

#### Acceptance Criteria

1. THE 代理服务 SHALL 以仓库根目录多文件 Go 源码实现全部逻辑（main.go / config.go / route.go / proxy.go / server.go，同属 package main），构建产物为静态链接单二进制 `mrp`，提供 android/arm64、ios/arm64、linux/arm64、windows/amd64 构建脚本
2. THE 代理服务 SHALL 从 YAML 配置文件（config.yaml）加载路由规则，按域名划分 server，路由按路径前缀匹配并指定上游；监听地址与 TLS 证书路径通过命令行参数指定
3. WHEN 收到 SIGHUP，THE 代理服务 SHALL 在已有连接不中断的情况下重新加载路由配置
4. IF 配置文件存在语法错误，THE 代理服务 SHALL 继续使用当前生效配置，并在日志输出错误行号与原因

### Requirement 2: 域名与路径路由

**User Story:** 作为联调人员，我希望按域名和路径前缀定义转发规则，并可选改写 Host 与路径，以便把请求路由到不同服务器的对应接口。

#### Acceptance Criteria

1. WHEN HTTP 请求到达，THE 代理服务 SHALL 依据 Host 头识别域名；WHEN TLS 连接到达，THE 代理服务 SHALL 依据 SNI 识别域名
2. THE 代理服务 SHALL 在匹配的 server 块内按最长路径前缀匹配 location 规则，并把请求转发到对应 proxy_pass 上游
3. WHEN 请求域名无匹配 server 块，THE 代理服务 SHALL 直接透传到原始目标
4. THE 代理服务 SHALL 在转发时保留原始请求的方法、路径、查询参数、请求头与请求体
5. THE 代理服务 SHALL 支持按 location 配置改写 Host 请求头与路径前缀映射
6. IF 上游连接失败或超时，THE 代理服务 SHALL 向客户端返回 502 状态码并在日志记录错误

### Requirement 3: HTTPS 支持

**User Story:** 作为联调人员，我希望代理服务用我预先创建的证书终结目标域名的 HTTPS 连接，以便按路径路由 HTTPS 请求。

#### Acceptance Criteria

1. WHEN 启动，THE 代理服务 SHALL 从命令行参数指定的路径加载 TLS 证书与私钥文件
2. WHEN 命令行通过 `--cert` / `--key` 指定证书而文件缺失或解析失败，THE 代理服务 SHALL 启动失败并输出明确的文件路径错误；未指定证书时仅支持 HTTP 转发与 CONNECT 隧道，TLS 直连连接被断开
3. WHEN 转发到 HTTPS 上游，THE 代理服务 SHALL 跳过上游证书校验，证书校验交由设备端信任的 CA 链路完成

### Requirement 4: 日志

**User Story:** 作为联调人员，我希望查看每个请求的转发明细，以便快速定位路由配置问题。

#### Acceptance Criteria

1. WHEN 请求完成转发，THE 代理服务 SHALL 记录该请求的域名、路径、匹配的规则、上游地址、响应状态码与耗时
2. THE 代理服务 SHALL 支持通过配置与命令行参数调整日志级别（debug/info/warn/error）

### Requirement 5: 证书操作文档

**User Story:** 作为使用者，我希望 README.md 提供证书创建与导入各设备的完整命令，以便自行完成证书准备工作。

#### Acceptance Criteria

1. THE README.md SHALL 提供通过 openssl 创建 CA 证书与覆盖全部目标域名的服务端证书（含 SAN）的完整命令
2. THE README.md SHALL 分别提供 Android、Windows、Linux 三类设备导入 CA 证书的操作步骤或命令
3. THE README.md SHALL 说明 Android 7.0 以上第三方 App 不信任用户证书的约束及系统证书存储的处理方式

## Constraints & Assumptions

- 单一端口监听：按连接首字节自动识别 HTTP / TLS，默认端口 `4000`；如需监听 `443` 等特权端口（Linux/Android）需 root 或 CAP_NET_BIND_SERVICE，Windows 无需提权；端口可通过 `--port` 调整
- 证书全部由使用者预先创建，程序仅加载；服务端证书 SAN 必须覆盖全部目标域名
- 客户端必须信任导入的 CA 证书：自研 App 可配置信任用户证书；第三方 App 需设备 Root 后装入系统证书存储；目标 App 已确认无证书绑定
- 流量如何到达代理服务（hosts、DNS 解析、显式代理设置）由使用者在设备侧自行配置，代理服务仅监听端口处理到达的请求
- HTTP 代理协议（CONNECT）在同一监听端口上一并支持，使服务可直接作为显式代理使用
