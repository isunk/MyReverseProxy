# AGENTS.md

mrp 项目代码规范。任何对本仓库的修改都应遵循以下约定。

## 命名风格

- 遵循 Go 惯例 camelCase：类型名大驼峰，变量、字段、函数、方法小驼峰。不用下划线分隔。
  - 正例：`transport`、`listenAddr`、`handler`、`handleConnect`、`byDomain`、`oneConnListener`、`statusRecorder`、`target`、`route.Table.InstallHandlers`
  - 反例：`trVerify`（缩写）、`transport_verify`（下划线）、`route_table`（下划线）
- 单词尽量完整，避免缩写：`transport` 不写 `tr`、`entry` 不写 `r`、`server` 不写 `s`。循环局部变量允许使用 `i`、`k`、`v` 等约定单字母，紧邻上下文允许 `r`（request）、`w`（writer）、`c`（conn）。
- 类型名大驼峰导出或小驼峰非导出（如 `route.Table`、`route.Entry`、`route.HeaderRewrite`、`cache.Cache`、`server.StatusRecorder`、`server.BufferedConn`、`dns.Resolver`、`ca.Authority`）。
- **YAML 反射字段**：`gopkg.in/yaml.v3` 要求结构体字段导出，采用大驼峰，如 `Config.Servers`（键 `servers`）、`Server.Domain`（键 `domain`）、`Route.Prefix`、`Route.Upstream`、`Route.Host`、`Headers.Request`/`Headers.Response`（键 `headers`）。改名须同步验证 `yaml` 键仍与既有配置文件兼容。
- **标准库接口方法**保持其原始拼写，如 `ServeHTTP`、`Accept`、`Close`、`Addr`、`WriteHeader`、`Unwrap`，因为须满足 `http.Handler` / `net.Listener` / `http.ResponseWriter` 等接口。

## 文件组织

- `mrp.bat` 为纯 ASCII（英文 UI）、CRLF 行尾的 Windows 分发脚本（仓库按原始字节存储）。为避免 cmd 批处理解析器按系统 ANSI 代码页读取脚本导致的编码错位/乱码，脚本刻意不使用中文与 `chcp`，故在任何代码页、任何终端（PowerShell 7、Windows Terminal、VSCode、双机 conhost）下都能正确解析与显示。修改后自检：仍是 CRLF 行尾、纯 ASCII（无 BOM、无任何非 ASCII 字节），禁止引入中文或加回 `chcp`。

代码按业界标准布局分层：`cmd/mrp` 是入口，业务逻辑全部在 `internal/*`，包之间单向依赖（`log`、`cache` 无内部依赖；`route`→`log`；`config`→`route,log`；`dns`→`cache,log`；`ca`→`cache,log`；`server`→`log`；`proxy`→`ca,config,dns,log,route,server`），禁止反向引用形成环。

| 文件 | 职责 | 不应包含 |
|------|------|----------|
| `cmd/mrp/main.go` | flags 解析（`parseStartupOptions`，含唯一监听端口 `--port` 与 `defaultListenPort=8000`）、`buildProxy`、信号循环、`run` | 路由/转发逻辑、监听细节、`os.Exit`（不可恢复失败走 `log.Fatal`） |
| `internal/config/config.go` | YAML 配置结构（`Config`/`Server`/`Route`/`Headers`）、`Load`、`Parse`、`Ensure`、`buildTable`、`buildServerEntry`、`buildRoutes`、`protocolLabel` | 任何运行期依赖、转发逻辑 |
| `internal/route/route.go` | `Route`、`Target`、`HeaderRewrite`、`Entry`、`Table`（含 `byDomain`）、`Pick`/`Has`/`Add`/`InstallHandlers`/`Fingerprint`、`ParseTarget`、`staticHandler`、`joinPath` | I/O、日志以外的运行期编排 |
| `internal/proxy/proxy.go` | `Proxy` 结构、`reload` 编排、`Handler`/`targetOf`/`hostAndPort`/`effectivePort`/`defaultPort`、`routeHandler`/`serveRequest`、`WatchFile` 热加载、`Reload`、`NewTransport`、`resetCaches` | CONNECT 隧道与 MITM 握手细节 |
| `internal/proxy/connect.go` | `handleConnect`/`hijackConn`/`serveConnect`/`tunnel` | 路由决策逻辑 |
| `internal/server/server.go` | `Serve`（始终明文监听，协议由请求内容推导）、`ServeSingleConn`、`oneConnListener`、`BufferedConn`、`StatusRecorder` | 路由决策逻辑 |
| `internal/ca/ca.go` | `Authority` 现场签发、`GetCertificate`、`ClearCache`、`Load` 证书加载与 CA 校验 | 路由决策逻辑 |
| `internal/dns/resolver.go` | 上游 DNS 解析与故障切换、`Resolver`（`New`/`Update`/`Addresses`/`DialContext`/`ClearCache`/`CacheSize`）、解析结果缓存 | 路由决策逻辑 |
| `internal/cache/cache.go` | 通用 `Cache` 并发缓存（`New`/`Get`/`Set`/`Evict`/`Clear`） | 具体业务逻辑 |
| `internal/log/log.go` | console 日志（`Debug`/`Info`/`Warn`/`Error`/`Fatal` 模板字符串、级别过滤、按级别整行着色、TTY 检测、`SetOutput`、`ParseLevel`） | 路由/转发逻辑 |
| `internal/testutil/` | 跨包共享测试替身：`ConfigFile`、`SelfSignedCert`、`AuthorityCA`、`WriteCertFiles`、`NewStub`（DNS）、`EchoServer`、`FreePort`、`MustURL` | 生产代码依赖（不得被非测试代码引用） |

每个包内配套 `<name>_test.go` 测试；`internal/proxy` 按主题拆分为 `proxy_test.go`、`connect_test.go`、`reload_test.go`、`transport_test.go`。新增文件时保持单一职责，文件名单词式小写。

## 代码风格

- `gofmt -w` 必须通过，提交前自检。
- `go vet ./...` 必须无告警。
- import 分组：标准库在前，内部包（`internal/*`）与第三方（`gopkg.in/yaml.v3`）同组在后，组间空行。
- 错误处理：可恢复错误 `return err` 并由调用方决策；不可恢复（启动失败、监听失败）走 `log.Fatal`，禁止在请求热路径内 `os.Exit`。
- 日志统一用 `log` 包的模板字符串方法输出（`Debug`/`Info`/`Warn`/`Error`/`Fatal`，`时间\t级别\t消息`），键值内嵌格式 `key=value`（`domain=%s`、`status=%d`），禁止 `fmt.Println` 调试残留。
- 热路径禁止注释，仅在解释"为什么"时保留简短中文注释；禁止冗余注释。
- 不引入额外依赖除非必要；当前仅依赖 `gopkg.in/yaml.v3` 与标准库。

## 并发与热加载

- 路由表通过 `atomic.Pointer[route.Table]` 持有，`reload` 整体替换，禁止对存量 `route.Table` 做原地修改。
- 监听端口由 `--port` 在启动时确定，`reload` 不重建监听；配置里的 `protocol`/`port` 只参与虚拟服务器匹配。
- 处理器在 `loadTable` 内由 `InstallHandlers` 构建完毕，此后路由表内容只读；运行期只做前缀匹配，不区分远程上游与本地目录。
- `reload` 失败时保留旧路由表与旧监听，仅记录错误，不影响存量连接。
- TLS 配置在启动时加载一次；路由配置文件修改后自动热加载（轮询变更），`SIGHUP` 手动触发仍可用，均只重载路由、不重载证书。

## 测试要求

- 新增/修改路由或配置逻辑须配套测试用例。
- 集成测试使用 `httptest` 模拟上游与随机端口，禁止依赖外部网络。
- 测试内证书通过 `testutil.SelfSignedCert` / `testutil.AuthorityCA` 现场生成，禁止硬编码证书文件。
- 跨包复用的测试替身统一放 `internal/testutil`；包内 helpers 按包就近命名，同样单词式：`recordingServer`、`startProxy`、`proxyClient`、`requestBody`。
- 测试内通过 `log.SetOutput(io.Discard)` 静音控制台输出，避免污染测试输出。

## 构建与验证

提交前需全部通过：

```bash
gofmt -w cmd internal
go vet ./...
go build -o mrp ./cmd/mrp
go test ./...
```

交叉编译纯静态目标：`android/arm64`、`linux/arm64`、`windows/amd64`，均 `CGO_ENABLED=0`；`ios/arm64` 需 Apple SDK 与 cgo 链接（以 `-buildmode=c-archive` 嵌入应用），不支持纯静态二进制。
