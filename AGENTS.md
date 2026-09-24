# AGENTS.md

mrp 项目代码规范。任何对本仓库的修改都应遵循以下约定。

## 命名风格

- 不使用小驼峰。变量、字段、函数、方法、类型名一律采用**单词式**命名，多词用下划线连接（snake_case）。
  - 正例：`transport_verify`、`route_table`、`http_listen`、`new_proxy`、`handle_connect`、`by_domain`
  - 反例：`trVerify`、`routeTable`、`httpListen`、`newProxy`、`handleConnect`、`byDomain`
- 单词尽量完整，避免缩写：`transport` 不写 `tr`、`entry` 不写 `r`、`server` 不写 `s`。循环局部变量允许使用 `i`、`k`、`v` 等约定单字母。
- 类型名同样单词式小写（如 `proxy`、`route_table`、`route_entry`、`log_writer`、`one_conn_listener`、`context_key`），项目内无跨包导出需求。
- **唯一例外**：需要被 `gopkg.in/yaml.v3` 反射注入的结构体字段必须导出，采用 PascalCase，如 `Domain`、`Routes`、`Prefix`、`Upstream`、`Host`、`TLSVerify`。
- **标准库接口方法**保持其原始拼写，如 `ServeHTTP`、`Accept`、`Close`、`Addr`、`WriteHeader`、`Unwrap`，因为须满足 `http.Handler` / `net.Listener` / `http.ResponseWriter` 等接口。

## 文件组织

按职责拆分，禁止把不相关逻辑塞进同一文件：

| 文件 | 职责 | 不应包含 |
|------|------|----------|
| `main.go` | flags 解析、信号循环、`run`、`fatal` | 路由/转发逻辑 |
| `config.go` | YAML 配置结构、`load_config` | 任何运行期依赖 |
| `route.go` | `route_entry`、`route_table`、`pick` | I/O、日志 |
| `proxy.go` | `proxy` 结构、`ServeHTTP`、`handle_connect`、`tunnel`、`reload`、转发构建 | 连接级 TLS 服务细节 |
| `server.go` | TLS 监听、SNI 注入、`one_conn_listener`、纯工具函数、`log_writer` | 路由决策逻辑 |
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

- 路由表通过 `atomic.Pointer[route_table]` 持有，`reload` 整体替换，禁止对存量 `route_table` 做原地修改。
- `reload` 失败时保留旧路由表，仅记录错误，不影响存量连接。
- TLS 配置在启动时加载一次，`SIGHUP` 只热加载路由配置，不重载证书。

## 测试要求

- 新增/修改路由或配置逻辑须配套测试用例。
- 集成测试使用 `httptest` 模拟上游与随机端口，禁止依赖外部网络。
- 测试内证书通过 `self_signed_cert` 辅助函数现场生成，禁止硬编码证书文件。
- 测试辅助函数命名同样单词式：`write_config_file`、`recording_server`、`start_proxy`、`proxy_client`、`request_body`。

## 构建与验证

提交前需全部通过：

```bash
gofmt -w *.go
go vet ./...
go build -o mrp .
go test ./...
```

交叉编译目标：`android/arm64`、`ios/arm64`、`linux/arm64`、`windows/amd64`，均 `CGO_ENABLED=0` 纯静态。
