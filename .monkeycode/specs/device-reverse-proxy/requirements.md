# Requirements Document

Feature Name: device-reverse-proxy
Updated: 2026-09-24

## Introduction

基于 Go 实现的单二进制反向代理服务。直接运行后监听 HTTP/HTTPS 端口，依据类 nginx 语法的配置文件，按请求的域名（SNI / Host）与路径前缀匹配自定义规则，将请求转发到不同的服务器。HTTPS 通过内置本地 CA 动态签发证书实现 TLS 终结。客户端流量通过 hosts / DNS 解析指向或显式代理设置到达本服务，流量引导由使用者自行完成，服务本身只做转发。

## Glossary

- **代理服务**: 本需求要开发的 Go 单二进制程序
- **目标域名**: 需要被劫持转发的域名
- **上游 (Upstream)**: 路由配置中 proxy_pass 指定的目标服务器
- **本地 CA**: 代理服务首次启动时自动生成的自签名根证书，持久化在配置目录
- **SNI**: TLS ClientHello 中的服务器名称指示，用于握手前识别目标域名

## Requirements

### Requirement 1: 启动与配置

**User Story:** 作为联调人员，我希望一个静态二进制加一份配置文件就能跑起来，并支持改配置不重启，以便快速部署与调整路由。

#### Acceptance Criteria

1. THE 代理服务 SHALL 以纯 Go 静态链接单二进制发布，提供 android/arm64、ios/arm64、linux/arm64、windows/amd64 构建产物
2. THE 代理服务 SHALL 从类 nginx 语法的配置文件加载监听端口与路由规则，server 块按域名划分，location 按路径前缀匹配，proxy_pass 指定上游
3. WHEN 收到 SIGHUP，THE 代理服务 SHALL 在已有连接不中断的情况下重新加载配置
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

**User Story:** 作为联调人员，我希望代理服务能终结目标域名的 HTTPS 连接，以便按路径路由 HTTPS 请求。

#### Acceptance Criteria

1. WHEN 首次启动且配置目录无 CA 证书，THE 代理服务 SHALL 生成自签名 CA 证书与私钥并持久化
2. WHEN TLS 连接到达，THE 代理服务 SHALL 解析 SNI，使用本地 CA 动态签发该域名的叶子证书并完成 TLS 握手
3. THE 代理服务 SHALL 提供 ca 子命令，将本地 CA 证书导出为 PEM 文件供安装到客户端设备
4. WHEN 转发到 HTTPS 上游，THE 代理服务 SHALL 默认校验上游证书链，并支持按 location 配置跳过校验

### Requirement 4: 日志

**User Story:** 作为联调人员，我希望查看每个请求的转发明细，以便快速定位路由配置问题。

#### Acceptance Criteria

1. WHEN 请求完成转发，THE 代理服务 SHALL 记录该请求的域名、路径、匹配的规则、上游地址、响应状态码与耗时
2. THE 代理服务 SHALL 支持通过配置与命令行参数调整日志级别（debug/info/warn/error）

## Constraints & Assumptions

- 监听 80/443：Linux/Android 需 root 或 CAP_NET_BIND_SERVICE，Windows 无需提权；端口可通过配置调整以规避
- HTTPS 终结要求客户端信任本地 CA：自研 App 可配置信任用户证书；第三方 App 需设备 Root 后装入系统证书存储；目标 App 已确认无证书绑定
- 流量如何到达代理服务（hosts、DNS 解析、显式代理设置）由使用者在设备侧自行配置，代理服务仅监听端口处理到达的请求
- HTTP 代理协议（CONNECT）在同一监听端口上一并支持，使服务可直接作为显式代理使用

## Open Questions

1. 无匹配域名透传时，HTTPS 流量按 SNI 原样转发即可，是否还需支持 CONNECT 隧道的非 TLS 端口透传？
