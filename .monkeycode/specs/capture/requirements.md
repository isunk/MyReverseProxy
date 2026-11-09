# Requirements Document

Feature Name: capture
Updated: 2026-11-09

## Introduction

为 mrp 增加抓包功能：把代理可见的明文 HTTP 交互（MITM 解密后的 HTTPS 请求与客户端直连的明文 HTTP 请求）记录为 HAR 1.2 文件，供 Chrome DevTools、Charles、Fiddler 等标准工具离线分析。抓包定位为联调期的诊断手段：输出文件落在设备本地、启动时按需开关，开启与否均不改变代理转发的行为。

## Glossary

- **抓包 (Capture)**: 把 mrp 可见的明文 HTTP 请求/响应交互记录为 HAR 文件的功能
- **HAR**: HTTP Archive 1.2 格式，JSON 结构的标准抓包存档格式，可被 Chrome DevTools / Charles / Fiddler 打开
- **明文交互**: mrp 能读到完整明文内容的 HTTP 请求/响应：MITM 解密后的隧道内层请求，以及客户端直接发来的明文 HTTP 请求
- **tunnel 流量**: 未命中配置域名时端到端加密的字节流，内容对 mrp 不可见
- **body 上限**: 单个请求/响应 body 记入 HAR 的最大字节数，超出部分截断
- **文件大小上限**: 抓包文件累计写入的最大字节数，达到后停止追加

## Requirements

### Requirement 1: 抓包开关与输出文件

**User Story:** 作为联调人员，我希望按需开关抓包并指定输出文件，以便只在排查问题时产生抓包数据。

#### Acceptance Criteria

1. WHEN mrp 启动时开启抓包，THE 代理服务 SHALL 把明文交互记录为 HAR 1.2 格式文件，输出文件路径可指定
2. WHEN mrp 启动时未开启抓包，THE 代理服务 SHALL 以零抓包开销运行，不产生抓包文件与记录动作
3. WHEN 抓包已开启，THE 代理服务 SHALL 每次运行新建输出文件从头记录（已存在同名文件截断），一次运行对应一份独立抓包文件
4. IF 输出文件无法创建（路径不可写等），THE 代理服务 SHALL 输出一条警告并以抓包关闭状态继续正常转发
5. IF 抓包文件累计大小达到文件大小上限，THE 代理服务 SHALL 停止追加记录、输出一条警告并继续正常转发

### Requirement 2: 记录内容

**User Story:** 作为联调人员，我希望每条交互包含完整的请求与响应细节，以便在标准 HAR 工具里还原现场。

#### Acceptance Criteria

1. WHEN 一次明文交互完成（响应已回传客户端），THE 代理服务 SHALL 追加一条 HAR entry，包含：开始时间、请求方法、完整 URL、HTTP 版本、请求头列表、请求 body 及其大小、响应状态码、响应头列表、响应 body 及其大小、总耗时
2. THE 代理服务 SHALL 按客户端实际收发的值记录请求头与响应头，同名多值头逐条记录
3. IF 请求或响应 body 超过 body 上限，THE 代理服务 SHALL 只记录上限内的前缀字节，并在该 entry 标注截断与原始大小
4. WHEN 同一客户端连接上发生多次交互（HTTP keep-alive），THE 代理服务 SHALL 逐次分别记录
5. THE 代理服务 SHALL 在 entry 中记录下游客户端地址与目标域名，便于按来源筛选

### Requirement 3: 记录范围

**User Story:** 作为联调人员，我希望抓包只覆盖 mrp 可见的明文流量，加密字节流不产生记录。

#### Acceptance Criteria

1. THE 代理服务 SHALL 记录全部明文交互：MITM 解密后的内层请求与客户端直接发来的明文 HTTP 请求
2. THE 代理服务 SHALL 排除 tunnel 流量的内容记录（tunnel 字节流不产生 HAR entry）
3. WHEN CONNECT 隧道建立（mitm started / tunnel started），THE 代理服务 SHALL 不为其生成 HAR entry

### Requirement 4: 转发行为不受影响

**User Story:** 作为使用者，我希望开启抓包后代理行为保持不变，以便抓包期间的业务验证结果可信。

#### Acceptance Criteria

1. THE 代理服务 SHALL 在响应回传客户端后完成该交互的记录（本地文件同步追加），记录动作不改变响应内容、状态码与头
2. IF 追加记录失败（磁盘满、IO 错误），THE 代理服务 SHALL 输出一条警告、停用抓包并继续正常转发
3. WHEN mrp 收到 SIGINT/SIGTERM 退出，THE 代理服务 SHALL 把 HAR 文件补全为合法 JSON（关闭 entries 数组与根对象）

### Requirement 5: 文档

**User Story:** 作为使用者，我希望 README 说明抓包的开关方式、文件位置与查看方法。

#### Acceptance Criteria

1. THE README.md SHALL 说明开启抓包的方式、输出文件默认路径、body 上限与文件大小上限
2. THE README.md SHALL 说明用 Chrome DevTools / Charles / Fiddler 打开 HAR 文件查看的方法
3. THE README.md SHALL 说明进程被强杀后 HAR 文件的手工修复方式

## Constraints & Assumptions

- 抓包开关与输出文件在启动时确定（与 `--cert` 同类的基础设施参数），不参与配置热加载
- body 上限与文件大小上限为内置常量（初值：body 1 MiB、文件 256 MB），初版不做成配置项
- HAR 文件为增量追加写入：进程被强杀时文件缺少尾部结构，可手工修复（README 说明）；正常退出时补全
- 抓包内容含 Authorization 等敏感头与业务数据，文件保留在设备本地，文件安全由使用者自行管理
- 零额外依赖：HAR 序列化仅用标准库 `encoding/json`
