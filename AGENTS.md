# AGENTS.md

mrp 项目代码规范。任何对本仓库的修改都应遵循以下约定。

## 命名风格

- 遵循 Go 惯例 camelCase：类型名大驼峰，变量、字段、函数、方法小驼峰。不用下划线分隔。
  - 正例：`transport`、`listenAddr`、`handler`、`handleConnect`、`group`、`oneConnListener`、`statusRecorder`、`target`、`route.Table.InstallHandlers`
  - 反例：`trVerify`（缩写）、`transport_verify`（下划线）、`route_table`（下划线）
- 单词尽量完整，避免缩写：`transport` 不写 `tr`、`entry` 不写 `r`、`server` 不写 `s`。循环局部变量允许使用 `i`、`k`、`v` 等约定单字母，紧邻上下文允许 `r`（request）、`w`（writer）、`c`（conn）。
- 类型名大驼峰导出或小驼峰非导出（如 `route.Table`、`route.Entry`、`route.HeaderRewrite`、`cache.Cache`、`frontend.StatusRecorder`、`frontend.BufferedConn`、`dns.Resolver`、`ca.Authority`）。
- **YAML 反射字段**：`gopkg.in/yaml.v3` 要求结构体字段导出，采用大驼峰，如 `Config.Servers`（键 `servers`）、`Server.Domain`（键 `domain`）、`Route.Prefix`、`Route.Upstream`、`Route.Host`、`Headers.Request`/`Headers.Response`（键 `headers`）。改名须同步验证 `yaml` 键仍与既有配置文件兼容。
- **标准库接口方法**保持其原始拼写，如 `ServeHTTP`、`Accept`、`Close`、`Addr`、`WriteHeader`、`Unwrap`，因为须满足 `http.Handler` / `net.Listener` / `http.ResponseWriter` 等接口。
- **同一概念只用一个词**：同一概念在包内只用一个词，不混入同义词。如日志级别阈值全用 `level`（`Level`/`ParseLevel`/`Init(level)`/`InfoLevel`/`settings.level`），不混入 `floor`。跨包同一结构模式若承载性质不同则各自用专业词：log 的 `settings`（配置）与 dns 的 `state`（运行态）同为 `atomic.Pointer` 快照目标，性质不同故不同名。
- **避免多义词与近邻标识符碰撞**：局部变量不得与紧邻代码的同词标识符碰撞。如快照变量 `now` 与同行 `time.Now()` 同词，应改名为 `current`。
- **优先领域专业词**：优先用领域通用专业词而非比喻。日志级别用 `level`（`logrus.Level`/`zap.Level`/`slog.Level`）不用 `floor`；HTTP 头写入用 `setHeaders` 对齐标准库 `http.Header.Set`，不用泛化动词 `apply`。
- **atomic.Pointer 快照命名**：存储变量按承载内容命名（如 `active`），快照局部变量用 `current`，对齐 `proxy.go` 的 `current := p.table.Load()`。

## 文件组织

- `mrp.hta` 为 UTF-8 编码的 Windows HTA 部署面板（MRP Helper），仓库按原始字节存储。渲染路径刻意保持全 ASCII：中文用数字实体、圆点用 `fromCharCode`，规避 HTA 宿主在不同系统区域设置下的编码错位；链接不带 `href`（`NAVIGABLE=no` 下 `javascript:` 会被交给默认浏览器），`@` 事件统一 `preventDefault`。双击运行生效，拖入浏览器仅显示界面、动作不可用。修改后自检：仍是 UTF-8 无 BOM、渲染文本全 ASCII（无中文字面量）。

代码按业界标准布局分层：`cmd/mrp` 是入口，业务逻辑全部在 `internal/*`，包之间单向依赖（`log`、`cache`、`capture`、`singleflight` 无内部依赖；`netutil`→`log`；`route`→`log,netutil`；`config`→`route,log`；`dns`→`cache,log,singleflight`；`ca`→`cache,log,singleflight`；`frontend`→`netutil`；`proxy`→`ca,capture,config,frontend,log,netutil,route`），禁止反向引用形成环。

| 文件 | 职责 | 不应包含 |
|------|------|----------|
| `cmd/mrp/main.go` | flags 解析（`parseStartupOptions`，含唯一监听端口 `--port` 与 `defaultListenPort=9000`）、`buildProxy`、信号循环、`run` | 路由/转发逻辑、监听细节、`os.Exit`（不可恢复失败走 `log.Fatal`） |
| `internal/config/config.go` | YAML 配置结构（`Config`/`Server`/`Route`/`Headers`）、`Load`、`Parse`、`Ensure`、`buildTable`、`validateDomainPattern`、`buildServerEntry`、`buildRoutes` | 任何运行期依赖、转发逻辑 |
| `internal/route/route.go` | `Route`、`Entry`、`Table`（`groups []*DomainGroup`）、`DomainGroup`、`Pick`/`Has`/`Add`/`InstallHandlers`/`Fingerprint`/`protocolLabel`/`pathHasPrefix` | I/O、日志以外的运行期编排 |
| `internal/route/target.go` | `Target`、`ParseTarget`/`validateURLHost`/`parseTargetHost`/`validPort`/`isLocalPath` | I/O、日志以外的运行期编排 |
| `internal/route/handler.go` | `HeaderRewrite`、`setHeaders`、`buildHandler`/`buildProxy`/`rewriteRequest`/`OutboundURL`、`UpstreamErrorHandler`、`staticHandler`、`statusHandler` | 路由匹配逻辑 |
| `internal/route/path.go` | `buildOutPath`/`singleJoiningSlash`（路径拼接与编码对齐） | 路由匹配逻辑 |
| `internal/proxy/proxy.go` | `Proxy` 结构、`reload` 编排、`Handler`/`Socks5`/`routeHandler`/`routeTarget`/`serveRequest`、`WatchFile` 热加载、`Reload`、`NewTransport`、`resetCaches`、`newPassthroughProxy`/`passthroughURL` | CONNECT 隧道与 MITM 握手细节、目标解析 |
| `internal/proxy/target.go` | `targetOf`/`normalizeProtocol`/`normalizeDomain`/`absoluteTarget`/`hostAndPort`/`hostOnly`/`trimIPv6Brackets`/`defaultPort`/`effectivePort`（请求目标解析） | 路由决策逻辑 |
| `internal/proxy/connect.go` | `handleConnect`/`hijackConn`/`serveConnect`/`dialTunnel`/`tunnel`/`mitmTLSConfig` | 路由决策逻辑 |
| `internal/proxy/capture.go` | `SetCapture`/`Close`、`headersFrom`、`decodeContent`/`isTextMimeType`/`binaryContent`、`recordCapture`/`contentTypeOf`（HAR entry 组装与内容解码） | 路由决策逻辑 |
| `internal/proxy/body.go` | `countingReadCloser`、`forwardedBody`、`newCapturedRequest`、`captureWriter`/`newCaptureWriter`、`captureRequestBody`（请求/响应体透传与抓包计数） | 路由决策逻辑 |
| `internal/proxy/resolver.go` | `resolver` 接口（`Update`/`Resolve`/`DialContext`/`ClearCache`），proxy 依赖抽象以注入测试替身 | 路由决策逻辑 |
| `internal/proxy/socks.go` | `serveSocks5`/`serveSocks5Connect`、`socks5Greeting`/`socks5ReadCommand`/`socks5ReadAddr`/`socks5DecodeHost`/`socks5WriteReply` | 路由决策逻辑 |
| `internal/proxy/socks_udp.go` | `serveSocks5UDP`/`socks5UDPRelay`/`socks5UDPSession`、`dialUDP`、`parseSocks5UDPHeader`/`encodeSocks5UDPResponse` | 路由决策逻辑 |
| `internal/frontend/frontend.go` | `Serve`（始终明文监听，协议由请求内容推导）、`ServeSingleConn`、`oneConnListener`、`BufferedConn`、`StatusRecorder`、`New` | 路由决策逻辑 |
| `internal/ca/ca.go` | `Authority` 现场签发、`GetCertificate`、`ClearCache`、`Load` 证书加载与 CA 校验 | 路由决策逻辑 |
| `internal/dns/resolver.go` | 上游 DNS 解析与故障切换、`Resolver`（`New`/`Update`/`Addresses`/`Describe`/`DialContext`/`Resolve`/`ClearCache`/`CacheSize`）、解析结果缓存 | 路由决策逻辑 |
| `internal/cache/cache.go` | 通用 `Cache` 并发缓存（`New`/`Get`/`Set`/`Evict`/`Clear`） | 具体业务逻辑 |
| `internal/singleflight/singleflight.go` | 泛型并发合并 `Group`（`Begin`/`Wait`/`WaitContext`/`Finish`），ca 签发与 dns 解析共用 | 缓存与具体业务逻辑 |
| `internal/capture/capture.go` | `Recorder` 增量写盘（`New`/`Record`/`Close`/`Path`）、文件大小上限、停用短路 | 具体业务逻辑 |
| `internal/capture/har.go` | HAR 1.2 结构体（`Har`/`Log`/`Entry`/`Request`/`Response`/`Content` 等） | 具体业务逻辑 |
| `internal/log/log.go` | console 日志（`Debug`/`Info`/`Warn`/`Error`/`Fatal` 模板字符串、`Init` 级别过滤、`DebugEnabled` 门控、按级别整行着色、TTY 检测、`SetOutput`、`ParseLevel`） | 路由/转发逻辑 |
| `internal/netutil/netutil.go` | `BufferPool`（32KB 拷贝缓冲池）、`Relay`（双向字节搬运）、`ServeListener`（Accept 循环与退避）、`UDPBufferSize` | 具体业务逻辑 |
| `internal/testutil/` | 跨包共享测试替身：`ConfigFile`、`SelfSignedCert`、`AuthorityCA`、`WriteCertFiles`、`NewStub`/`NewStubWithTTL`（DNS）、`EchoServer`、`UDPEchoServer`、`FreePort`、`MustURL`、`ClosedUDPPort`/`SilentUDPPort` | 生产代码依赖（不得被非测试代码引用） |

每个包内配套 `<name>_test.go` 测试；`internal/proxy` 按主题拆分为 `proxy_test.go`、`connect_test.go`、`reload_test.go`、`transport_test.go`、`capture_test.go`、`socks_test.go`、`socks_udp_test.go`、`proxy_bench_test.go`（HTTP/MITM/隧道性能基准）。新增文件时保持单一职责，文件名单词式小写。

## 代码风格

- `gofmt -w` 必须通过，提交前自检。
- `go vet ./...` 必须无告警。
- import 分组：标准库在前，内部包（`internal/*`）与第三方（`gopkg.in/yaml.v3`）同组在后，组间空行。
- 错误处理：可恢复错误 `return err` 并由调用方决策；不可恢复（启动失败、监听失败）走 `log.Fatal`，禁止在请求热路径内 `os.Exit`。
- 日志统一用 `log` 包的模板字符串方法输出（`Debug`/`Info`/`Warn`/`Error`/`Fatal`，`时间\t级别\t消息`），键值内嵌格式 `key=value`（`domain=%s`、`status=%d`），禁止 `fmt.Println` 调试残留。
- 热路径禁止注释，仅在解释"为什么"时保留简短中文注释；禁止冗余注释。
- 不引入额外依赖除非必要；当前仅依赖 `gopkg.in/yaml.v3` 与标准库。
- **禁止纯委托包装方法**：方法体仅转发到另一函数（仅绑定字段或逐参透传）属多余封装，在调用点直接调用。合法委托仅三种：未导出字段的访问器（`Route.Handler()` 返回 `r.handler`、`Resolver.ClearCache()` 转发 `r.cache.Clear()`）、接口方法实现（`net.Listener.Close`/`Addr`、`net.Conn.Read`）、exported/unexported API 边界（须注释说明意图，如 `Proxy.Reload`→`reload`）。
- **禁止冗余闭包**：方法签名与目标类型一致时直接用方法值，不包匿名闭包。如 `http.HandlerFunc(p.serveRequest)` 优于 `http.HandlerFunc(func(w,r){ p.serveRequest(w,r) })`。
- **跨类型共享辅助用自由函数**：被多个类型复用的辅助操作用包级自由函数，不挂为单一类型的方法。如 `setHeaders` 同时服务于 `Route.rewriteRequest`、`buildProxy` 的 `ModifyResponse`、`staticHandler`、`statusHandler`，故为自由函数而非 `HeaderRewrite` 方法。
- **成功路径提前返回**：成功分支提前 `return`，错误回退留到末尾，避免「先拷贝再条件覆盖」的绕行写法。
- **死赋值用 `_` 丢弃**：有意不用的返回值赋给 `_`（`_, code = connect(...)`），不保留命名变量引发 staticcheck SA4006。
- **热路径 Debug 日志须门控**：`log.Debug` 的参数在调用前求值，即使级别过滤也会执行 `fmt.Sprintf` 与参数构造。热路径上 Debug 日志的参数含字符串拼接或函数调用时，须以 `log.DebugEnabled()` 门控，避免生产默认 Info 级别下每请求白跑。

## 并发与热加载

- 路由表通过 `atomic.Pointer[route.Table]` 持有，`reload` 整体替换，禁止对存量 `route.Table` 做原地修改。
- 热路径无锁读取、冷路径整体替换的可变配置经 `atomic.Pointer` 持有：字段收进不可变结构体，`Load` 取快照后只读字段，`Store` 整体替换。路由表（`atomic.Pointer[route.Table]`）与日志配置（`atomic.Pointer[settings]`）均循此模式；`logf` 单次原子读快照后无锁判断级别与着色，仅写输出时持 `logMu`。
- 监听端口由 `--port` 在启动时确定，`reload` 不重建监听；配置里的 `protocol`/`port` 只参与虚拟服务器匹配。
- 处理器在 `reload` 内由 `InstallHandlers` 构建完毕，此后路由表内容只读；运行期只做前缀匹配，不区分远程上游与本地目录。
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
staticcheck ./...
go build -o mrp ./cmd/mrp
go test ./...
```

交叉编译纯静态目标：`android/arm64`、`linux/arm64`、`windows/amd64`，均 `CGO_ENABLED=0`；`ios/arm64` 需 Apple SDK 与 cgo 链接（以 `-buildmode=c-archive` 嵌入应用），不支持纯静态二进制。
