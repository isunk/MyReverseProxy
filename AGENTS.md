# AGENTS.md

mrp 项目代码规范。任何对本仓库的修改都应遵循以下约定。

## 命名风格

- 遵循 Go 惯例 camelCase：类型名大驼峰，变量、字段、函数、方法小驼峰。不用下划线分隔。
  - 正例：`transportVerify`、`routeTable`、`listenAddr`、`handleConn`、`handleConnect`、`byDomain`、`oneConnListener`、`logWriter`
  - 反例：`trVerify`（缩写）、`transport_verify`（下划线）、`route_table`（下划线）
- 单词尽量完整，避免缩写：`transport` 不写 `tr`、`entry` 不写 `r`、`server` 不写 `s`。循环局部变量允许使用 `i`、`k`、`v` 等约定单字母，紧邻上下文允许 `r`（request）、`w`（writer）、`c`（conn）。
- 类型名大驼峰导出或小驼峰非导出（如 `route`、`routeTable`、`routeEntry`、`logWriter`、`oneConnListener`、`ctxKey`）。
- **YAML 反射字段**：`gopkg.in/yaml.v3` 要求结构体字段导出，采用大驼峰，如 `Domain`、`Routes`、`Prefix`、`Upstream`、`Host`、`TLSVerify`。
- **标准库接口方法**保持其原始拼写，如 `ServeHTTP`、`Accept`、`Close`、`Addr`、`WriteHeader`、`Unwrap`，因为须满足 `http.Handler` / `net.Listener` / `http.ResponseWriter` 等接口。

## 文件组织

按职责拆分，禁止把不相关逻辑塞进同一文件：

| 文件 | 职责 | 不应包含 |
|------|------|----------|
| `main.go` | flags 解析、信号循环、`run`、`fatal` | 路由/转发逻辑 |
| `config.go` | YAML 配置结构、`loadTable` | 任何运行期依赖 |
| `route.go` | `route`、`routeTable`、`pick` | I/O、日志 |
| `proxy.go` | `proxy` 结构、`ServeHTTP`、`handleConnect`、`tunnel`、`reload`、`watchFile` 热加载、转发构建 | 连接级 TLS 服务细节 |
| `server.go` | 单端口监听、TLS/HTTP 协议识别、SNI 注入、`oneConnListener`、纯工具函数、`logWriter` | 路由决策逻辑 |
| `main_test.go` | 单元与集成测试 | — |

新增文件时保持单一职责，文件名单词式小写。

## 代码风格

- `gofmt -w` 必须通过，提交前自检。
- `go vet ./...` 必须无告警。
- import 分组：标准库在前，第三方（`gopkg.in/yaml.v3`）在后，组间空行。
- 错误处理：可恢复错误 `return err` 并由调用方决策；不可恢复（启动失败、监听失败）走 `fatal`，禁止在请求热路径内 `os.Exit`。
- 日志统一用 `log/slog` 结构化输出，键值用单词式（`domain`、`path`、`upstream`、`status`、`elapsed`），禁止 `fmt.Println` 调试残留。
- 热路径禁止注释，仅在解释"为什么"时保留简短中文注释；禁止冗余注释。
- 不引入额外依赖除非必要；当前仅依赖 `gopkg.in/yaml.v3` 与标准库。

## 并发与热加载

- 路由表通过 `atomic.Pointer[routeTable]` 持有，`reload` 整体替换，禁止对存量 `routeTable` 做原地修改。
- `reload` 失败时保留旧路由表，仅记录错误，不影响存量连接。
- TLS 配置在启动时加载一次；路由配置文件修改后自动热加载（轮询变更），`SIGHUP` 手动触发仍可用，均只重载路由、不重载证书。

## 测试要求

- 新增/修改路由或配置逻辑须配套测试用例。
- 集成测试使用 `httptest` 模拟上游与随机端口，禁止依赖外部网络。
- 测试内证书通过 `selfSignedCert` 辅助函数现场生成，禁止硬编码证书文件。
- 测试辅助函数命名同样单词式：`writeConfigFile`、`recordingServer`、`startProxy`、`proxyClient`、`requestBody`。

## 构建与验证

提交前需全部通过：

```bash
gofmt -w *.go
go vet ./...
go build -o mrp .
go test ./...
```

交叉编译纯静态目标：`android/arm64`、`linux/arm64`、`windows/amd64`，均 `CGO_ENABLED=0`；`ios/arm64` 需 Apple SDK 与 cgo 链接（以 `-buildmode=c-archive` 嵌入应用），不支持纯静态二进制。
