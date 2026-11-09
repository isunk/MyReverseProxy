# Requirements Document

## Introduction

为 mrp 的 YAML 配置增加域名通配符匹配能力。当前 `domain` 仅支持精确匹配（`internal/route/route.go` 的 `Table.Pick`/`Table.Has` 按 map key 精确查找），用户希望一条通配符配置即可覆盖一组域名，典型场景：让所有 HTTPS 请求都走 MITM 模式。

## Glossary

- **虚拟服务器**：一条 `servers[]` 配置项，由 (domain, protocol, port) 组合标识，承载一组路由。
- **精确域名**：不含 `*` 的域名值，保持既有精确匹配语义。
- **通配符模式**：以 `*` 匹配域名的配置值，本功能引入。
- **MITM**：CONNECT 隧道命中虚拟服务器时，代理现场签发证书并解析隧道流量的模式。
- **透传隧道**：CONNECT 隧道未命中虚拟服务器时，字节级双向转发。

## Requirements

### Requirement 1: 通配符模式配置

**User Story:** AS 用户，I want 在 YAML 的 domain 字段使用通配符，so that 一条配置即可覆盖一组域名而无需逐个枚举。

#### Acceptance Criteria

1. WHEN 配置项的 domain 值为 `*`，the system SHALL 将该虚拟服务器应用于任意请求域名。
2. WHEN 配置项的 domain 值为 `*.suffix` 形式，the system SHALL 将该虚拟服务器应用于形如 `label.suffix` 的请求域名，其中 label 为非空且不含 `.` 的单层标签。
3. WHEN 配置项的 domain 值为 `*.suffix` 形式且请求域名为裸域 `suffix`，the system SHALL 判定该虚拟服务器不匹配裸域。
4. IF domain 值在 `*` 之外还包含其他通配符位置（如 `a.*.com`、`*example.com`、`*.*`）或 `*` 后缀为空（`*.`），the system SHALL 拒绝加载配置并报告包含该值的错误。
4. WHEN domain 值为精确域名，the system SHALL 保持既有精确匹配行为与日志输出。
5. WHEN domain 值包含 `*`，the system SHALL 对模式做与精确域名一致的小写与空白归一。

### Requirement 2: 匹配优先级

**User Story:** AS 用户，I want 虚拟服务器的匹配优先级由 YAML 中 `servers` 的声明顺序决定，so that 我可以用声明顺序显式控制哪条配置先生效。

#### Acceptance Criteria

1. WHEN 请求命中多个虚拟服务器，the system SHALL 选用 `servers` 数组中声明顺序最先的命中项。
2. WHEN 用户把通配符模式声明在精确域名之前，the system SHALL 按声明顺序让通配符模式先于精确域名生效。
3. WHEN 同一 domain 值在 `servers` 中出现多次，the system SHALL 按既有语义合并为同一分组的多个 (protocol, port) 条目。
4. IF 同一 domain 值下 (protocol, port) 组合重复，the system SHALL 拒绝加载配置并报告错误（含通配符模式在内）。
5. WHEN 请求域名为 IP 字面量（如 `127.0.0.1`、`[::1]`），the system SHALL 仅允许 domain 值为 `*` 的虚拟服务器命中。

### Requirement 3: 路由与 MITM 行为一致

**User Story:** AS 用户，I want 通配符命中的虚拟服务器与精确域名命中时行为完全一致，so that 无需为两种配置方式维护两套心智模型。

#### Acceptance Criteria

1. WHEN 请求域名命中通配符虚拟服务器，the system SHALL 按其 (protocol, port) 筛选与最长前缀匹配路由，行为与精确域名命中一致。
2. WHEN CONNECT 目标域名命中任意虚拟服务器（含通配符），the system SHALL 建立 MITM 隧道并按 CONNECT 目标域名签发证书。
3. WHEN CONNECT 目标域名未命中任何虚拟服务器，the system SHALL 保持透传隧道。

### Requirement 4: 热加载与指纹

**User Story:** AS 用户，I want 通配符配置参与既有热加载机制，so that 修改配置后行为自动更新。

#### Acceptance Criteria

1. WHEN 含通配符模式的配置文件变化触发热加载，the system SHALL 按新路由表整体替换并清理派生缓存。
2. WHEN 路由表指纹未变化，the system SHALL 跳过处理器重建与缓存清理。
