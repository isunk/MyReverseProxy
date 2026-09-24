# Requirements Document

Feature Name: device-reverse-proxy
Updated: 2026-09-24

## Introduction

基于 Go 实现的单二进制本地反向代理服务，可直接在移动设备（Android / iOS / HarmonyOS）的 shell 环境中运行。服务以代理服务器方式劫持目标 App 对固定域名（一个或多个）的请求，依据类 nginx 语法的配置文件将请求反向代理转发到自有服务器。内置 Fake IP DNS 劫持、本地 CA 动态签发证书的 TLS 中间人能力，并提供多种流量接管模式以适配不同权限环境（root / 无 root）。

## Glossary

- **目标域名**: 需要被劫持转发的域名，来自目标 App 内部固定访问的域名列表
- **目标 App**: 现有的、发出请求的移动端应用
- **代理服务**: 本需求要开发的 Go 单二进制程序
- **上游 (Upstream)**: 路由配置中 proxy_pass 指定的自有服务器地址
- **本地 CA**: 代理服务首次启动时自动生成的自签名根证书，持久化在配置目录
- **假 IP (Fake IP)**: DNS 劫持模式下为目标域名返回的保留网段（198.18.0.0/15）内虚拟地址
- **SNI**: TLS ClientHello 中的服务器名称指示，用于握手前识别目标域名
- **接管模式**: 流量进入代理服务的方式，包括显式代理、DNS 劫持、TUN、iptables 透明代理

## Requirements

### Requirement 1: 单二进制与配置加载

**User Story:** 作为联调人员，我希望把一个静态二进制推到设备上就能运行，并用类 nginx 语法的配置文件定义路由，以便低成本地在各设备上部署和调整。

#### Acceptance Criteria

1. THE 代理服务 SHALL 以纯 Go 静态链接单二进制发布，提供 android/arm64、ios/arm64、linux/arm64 三种构建产物
2. THE 代理服务 SHALL 从类 nginx 语法的配置文件加载路由规则，server 块按域名划分，location 按路径前缀匹配，proxy_pass 指定上游
3. WHEN 收到 SIGHUP 信号或调用 reload 管理接口，THE 代理服务 SHALL 在已有连接不中断的情况下重新加载配置
4. IF 配置文件存在语法错误，THE 代理服务 SHALL 继续使用当前生效配置，并在日志中输出错误行号与原因

### Requirement 2: 多模式流量接管

**User Story:** 作为联调人员，我希望代理服务提供多种流量接管模式，以便在不同权限环境（root / 无 root）的设备上都能完成劫持。

#### Acceptance Criteria

1. THE 代理服务 SHALL 提供显式代理模式，监听指定端口并处理 HTTP 代理协议（含 HTTPS CONNECT）
2. THE 代理服务 SHALL 提供 DNS 劫持模式，监听 UDP 53 端口，对目标域名返回假 IP，对其余域名向真实 DNS 转发
3. WHEN 以 TUN 模式运行，THE 代理服务 SHALL 创建 TUN 网卡，通过用户态 TCP/IP 协议栈把目标流量送入本地路由处理
4. WHEN 以 iptables 透明代理模式运行，THE 代理服务 SHALL 从内核连接信息中还原原始目标地址并送入本地路由处理
5. IF 进程缺少创建 TUN 所需权限，THE 代理服务 SHALL 输出提示并以显式代理模式降级运行
6. THE 代理服务 SHALL 监听 80 与 443 端口，用于接收假 IP 流量（配合 DNS 劫持或设备路由指向）

### Requirement 3: 类 nginx 路由与转发

**User Story:** 作为联调人员，我希望按域名和路径前缀定义转发规则，并可选改写 Host 与路径，以便把请求准确路由到自有服务器的对应接口。

#### Acceptance Criteria

1. WHEN 请求进入代理服务，THE 代理服务 SHALL 依据 SNI（TLS 连接）或 Host 头（HTTP 连接）识别域名并匹配 server 块
2. THE 代理服务 SHALL 在 server 块内按最长路径前缀匹配 location 规则，并把请求转发到对应 proxy_pass 上游
3. WHEN 域名或路径均无匹配规则，THE 代理服务 SHALL 按配置的默认行为处理（直连原目标或拒绝，二选一可配置）
4. THE 代理服务 SHALL 在转发时保留原始请求的方法、路径、查询参数、请求头与请求体
5. THE 代理服务 SHALL 支持按 location 配置改写 Host 请求头与路径前缀映射
6. IF 上游连接失败或超时，THE 代理服务 SHALL 向客户端返回 502 状态码并在日志记录错误

### Requirement 4: TLS 中间人

**User Story:** 作为联调人员，我希望代理服务能终结目标域名的 TLS 连接，以便读取和路由 HTTPS 请求。

#### Acceptance Criteria

1. WHEN 首次启动且配置目录无 CA 证书，THE 代理服务 SHALL 生成自签名 CA 证书与私钥并持久化
2. WHEN TLS 连接到达，THE 代理服务 SHALL 解析 SNI 获取域名，使用本地 CA 动态签发该域名的叶子证书并完成 TLS 握手
3. WHEN 转发到 HTTPS 上游，THE 代理服务 SHALL 默认校验上游证书链，并支持按配置跳过校验

### Requirement 5: 日志与管理接口

**User Story:** 作为联调人员，我希望查看每个被劫持请求的转发明细并支持运行时管理，以便快速定位路由配置问题。

#### Acceptance Criteria

1. WHEN 请求完成转发，THE 代理服务 SHALL 记录该请求的域名、路径、匹配的规则、上游地址、响应状态码与耗时
2. THE 代理服务 SHALL 提供仅监听本机回环地址的 HTTP 管理接口，支持 reload、stats、日志级别调整
3. THE 代理服务 SHALL 支持通过配置调整日志级别（debug/info/warn/error）

### Requirement 6: 证书导出

**User Story:** 作为联调人员，我希望导出本地 CA 证书文件，以便安装到设备证书存储中使 TLS 中间人生效。

#### Acceptance Criteria

1. THE 代理服务 SHALL 提供 ca 子命令，将本地 CA 证书导出为 PEM 文件到指定路径
2. IF 配置目录已存在 CA，THE 代理服务 SHALL 直接复用已有 CA，保证证书持久一致

## Constraints & Assumptions

- Android：二进制经 adb push 或 Termux 运行；TUN 模式与 iptables 模式需要 root；显式代理模式无需 root
- iOS：未越狱设备无法运行任意 shell 二进制，本方案仅适用于越狱设备；非越狱场景需另建 NetworkExtension App（超出本期范围）
- HarmonyOS NEXT：计划以 GOOS=linux 纯静态二进制经 hdc shell 运行，可行性待设备实测验证，列入风险项
- 目标 App 必须信任本地 CA 才能完成 TLS 中间人：自研 App 可配置信任用户证书；第三方 App 需设备已 Root 并将 CA 装入系统证书存储
- TUN 模式与设备上其他 VPN 应用互斥
- 目标 App 已确认可安装根证书且无证书绑定（Pinning）

## Open Questions

1. Android 联调设备是否已 Root（决定默认接管模式选 TUN/iptables 还是显式代理）？
2. HarmonyOS 目标版本是 NEXT 还是 4.x 及以下（4.x 兼容 Android 运行时，可直接复用 android 产物）？
3. 无匹配规则流量的默认行为选直连还是拒绝？
