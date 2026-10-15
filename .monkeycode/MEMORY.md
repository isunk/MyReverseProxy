# User Instruction Memory

This file records user instructions, preferences, and teachings for reference in future interactions.

## Format

### User Instruction Entry
[User Instruction Summary]
- Date: [YYYY-MM-DD]
- Context: [Mentioned scenario or time]
- Instructions:
  - [Content of user teaching or instruction, described line by line]

### Project Knowledge Entry
[Project Knowledge Summary]
- Date: [YYYY-MM-DD]
- Context: Discovered by Agent while performing [specific task description]
- Category: [Operations & Deployment|Build Methods|Testing Methods|Troubleshooting & Debugging|Workflow & Collaboration|Environment Configuration]
- Instructions:
  - [Specific knowledge points, described line by line]

## Deduplication Strategy
- Before adding a new entry, check for similar or identical instructions.
- If a duplicate is found, skip the new entry or merge it with the existing one.
- When merging, update the context or date information.
- This helps avoid redundant entries and keeps the memory file tidy.

## Entries

[User Instruction Summary]
- Date: 2026-09-30
- Context: Trimming the new "功能特性" section of README.md after the first draft was too long
- Instructions:
  - Keep README feature lists short and user-facing: only what a user picks mrp for. Drop implementation-level items — single-port listening, read-header/idle timeouts, DNS/connection caching, connection-pool sizes, TLS session cache size, log levels and ANSI colors, error-code details, build targets and deployment mechanics (the 使用流程 and mrp.bat sections already cover those).
  - Cache/TTL behavior belongs only in the config field table and the CLI flag table, where real fields and flags are documented, not in the feature list.
  - Merge small groups: request/response header rewriting lives under 路由.
  - The user cares most about the DNS-query feature, so it keeps its own subsection, but with a single line: mrp resolves upstream domains itself using the configured DNS servers instead of the device system DNS. Failover across multiple servers was also removed as too detailed. Everything deeper about `nameservers` belongs in 配置说明, not here.
  - No boundary or resilience statements belong in the feature list either: things like "mrp does not itself offer a DNS service" and "a failed reload does not affect existing connections" were both removed on request. Negative/boundary claims and failure-mode guarantees are operational caveats, not features.
  - The README title is `# My Reverse Proxy`. The user rejected the earlier `我的反向代理 (mrp)` as sounding unprofessional.
  - The 功能特性 section carries only short, plain functional statements: no SNI/CA/ECDSA wording, no flag names, no timeouts, no port counts, no protocol-internal details (UDP/TCP, IPv4/IPv6 ordering), no file-path or error-message text, no 1s polling or SIGHUP. If it can be stated in one plain sentence, state it that way.
  - Detailed config semantics, pitfalls (the `/etc/resolv.conf` -> `[::1]` refusal), syntax examples and flag defaults live in the 配置说明 field table, the 启动服务 flag table, and the config template comments. Do not repeat them in the feature list.

[Project Knowledge Summary]
- Date: 2026-09-29
- Context: Discovered by Agent while restructuring the mrp.bat device menus to show unified status lines for Android (adb) and HarmonyOS (hdc)
- Category: Troubleshooting & Debugging
- Instructions:
  - HarmonyOS hdc has no verified global-HTTP-proxy *query* command. The set path is `hdc shell network-cfg set http_proxy <host:port>`, but no equivalent `get` was confirmable from official docs. The user explicitly chose to leave hdc proxy status as `Unknown` rather than wire in a guessed command. Do not add hdc proxy probing with an unverified command; only adb is probed (`adb shell settings get global http_proxy`, compared to `127.0.0.1:<PORT>`).
  - `adb shell` and `hdc shell` both merge device-side stderr into the host-side stdout, and exit codes are unreliable. Remote file-existence checks must therefore use a command that emits nothing on failure (`test -f %1 && echo Y`, judged by temp-file size) — never `ls %1`, whose error line leaks into stdout and makes the path always look present. `pidof <bin>` is safe because it prints nothing when no process matches.

[User Instruction Summary]
- Date: 2026-10-03
- Context: 用户先后提出「优化变量命名」「仅优化函数名」，两次均在实现完成、门禁全绿后以「撤销修改」「撤销」要求全部还原（回到 a6321cd）
- Instructions:
  - 纯命名层面的重构（变量、接收者、函数名）风险收益比低：用户想先看改动方案，最终未必采纳。此类任务先输出改名对照表 + diff 预览请用户确认，确认后再落地，避免改完再撤。
  - 不主动提议命名优化；mrp 现有命名被视为已足够。已确认的命名约定以 AGENTS.md 为准，不再额外收紧。
  - 撤销指令用 `git restore .` 回到最近提交即可（mrp 仓库工作区平时保持干净）。

[Project Knowledge Summary]
- Date: 2026-09-29
- Context: Discovered by Agent while measuring mrp proxy latency and TLS/DNS overhead during a performance optimization pass
- Category: Testing Methods
- Instructions:
  - The sandbox is `linux/amd64` (Intel Xeon, localhost) but mrp ships on Android/Arm. Absolute latency and throughput figures measured here are only directional; do not present them as target-device numbers without re-measuring on a real device.
  - To isolate one optimization from the rest of the proxy path, force a fresh upstream connection per request (`transport.DisableKeepAlives = true`, `MaxIdleConns = 0`, `MaxIdleConnsPerHost = 0`) while reusing the client connection; measuring through a fully cold client+transport each iteration produces GC noise (~0.7ms swing) that swamps sub-millisecond gains.
  - TLS session resumption tests are order-sensitive: the session ticket is a TLS 1.3 post-handshake message, so `DidResume` stays 0 unless the response body is actually read. Reusing a single TCP connection for a second TLS handshake always fails (`tls: first record does not look like a TLS handshake`) — it is not a viable way to simulate connection reuse.
  - A benchmark that does not read and close the response body never returns the connection to the pool, so every iteration pays a fresh TCP+TLS handshake: the same path measured 1.93 ms instead of 25 µs. Always `io.ReadAll` + `Close` inside the timed loop.
  - Benchmarking the CONNECT tunnel: do not `CloseWrite()` and read to EOF. mrp's `tunnel` closes the upstream connection as soon as the client->upstream copy hits EOF, so a half-closed client truncates the upstream reply (measured 96 KB of 1 MB). Read an exact byte count with `io.CopyN` and leave the connection open.

[Project Knowledge Summary]
- Date: 2026-09-29
- Context: Discovered by Agent while deciding which of the remaining performance candidates to ship in mrp
- Category: Troubleshooting & Debugging
- Instructions:
  - Performance optimization in mrp is considered done. The two items that measured real, shippable gains are the DNS resolution cache (`--dns-ttl`, 151µs → 44µs per dial) and upstream TLS session resumption (`ClientSessionCache`, ~1.94ms → 878µs per upstream TLS connection). Do not re-propose the ones below; each was measured and rejected:
  - Tunnel connection pooling is impossible, not just unhelpful: TLS session state is bound to a single connection, so a second handshake on a reused TCP connection fails. Reuse at the client side is already provided for free by mrp.
  - Client-facing HTTP/2 would require `golang.org/x/net/http2` (stdlib has no `http2.ConfigureServer`), breaking the zero-third-party-dependency rule. It also does not help the dominant path: Android apps reach mrp as an HTTP proxy via `CONNECT`, where mrp relays bytes and cannot participate at the HTTP layer.
  - Streaming/SSE flushing needs no change: Go's `httputil.ReverseProxy` auto-flushes `text/event-stream` and `ContentLength == -1` responses, and upstream HTTP/2 is already on via `ForceAttemptHTTP2`.
  - The per-request logging path is the largest purely-mrp allocation cluster left (`logInfo` = 820 ns, 5 allocs, 392 B; `formatLogLine` = 448 ns, 4 allocs) but is ~1.3% of the 62 µs total proxy overhead, so it is not worth adding a `sync.Pool`.
  - Hardcoded `ResponseHeaderTimeout=30s` and `tunnelDialTimeout=10s` were deliberately left as constants rather than flags: 30 s for response *headers* is already generous for a mobile-network proxy, and disabling it would hang on dead upstreams. The DNS half of the timeout is already tunable via `--dns-timeout`.
  - Second pass (`internal/proxy/proxy_bench_test.go`, full real MITM chain with genuine client-side cert verification): mrp steady state is 69 µs/request at 179 allocs vs 25 µs for an identical upstream reached directly — about 44 µs of mrp overhead, dominated by upstream RTT, not mrp. First-ever request 4.2 ms, repeat cold request 1.4–1.75 ms, one cert signing 166 µs. Tunnel pass-through 290 MB/s on loopback, so the 32 KB `io.Copy` buffer is not worth enlarging.
  - The one real stall measured: `dns.resolve` tries nameservers serially with a 1 s per-server cap, so a first nameserver that silently drops queries costs **1.001 s** on the first request for a new domain (a nameserver that refuses costs 0 ms and switches instantly). Public DNS silently dropped by NAT/firewall is the common shape of this. The user chose to keep the serial order, so the configured order remains a priority semantic and the README wording "按配置顺序故障切换" stays correct. Do not convert `resolve` to concurrent racing.
  - `NewTransport` builds on `http.DefaultTransport.Clone()`, which silently inherits `ExpectContinueTimeout=1s`, `IdleConnTimeout=90s`, `TLSHandshakeTimeout=10s` and `ForceAttemptHTTP2=true`. Left as-is: the 100-continue wait was never reproducible on loopback (a Go handler reading the request body auto-sends 100 Continue), so `ExpectContinueTimeout=0` has no measured benefit.

[Project Knowledge Summary]
- Date: 2026-09-30
- Context: Discovered by Agent while mapping module responsibilities, dependency edges and leftover optimisation candidates for the mrp codebase
- Category: Troubleshooting & Debugging
- Instructions:
  - Module boundaries and dependency direction were verified against the source and match AGENTS.md exactly: `main` → `proxy, server, config, ca, dns, log`; `proxy` → `ca, config, dns, log, route, server`; `config` → `route, log`; `ca`/`dns` → `cache, log`; `route` → `log`; `cache`/`log`/`server` have no internal imports. `internal/testutil` is referenced only by `_test.go` files. Production code is ~1790 lines against ~2730 lines of tests.
  - Do not propose removing `p.authority.ClearCache()` from `Proxy.resetCaches`. The CA is loaded once at startup and reload never touches `tlsConfig`, so the clear drops still-valid certs (166 µs of re-signing on the next unseen SNI domain), but the user judged reload low frequency and treating stale derived state as throwaway as intended behaviour. The DNS cache clear in the same call is also covered by the `host:port` cache key and is fixed as a contract by `TestReload_ClearsDNSCache`.
  - `route.Table.Pick` stays a linear scan, and `frontend.ServeSingleConn` keeps allocating a per-connection `*http.Server`: hijack detection needs a per-connection `ConnState`, and sharing one server would race across connections. Both were reviewed and deliberately left.
  - Latent data race in the `log` package: `logFloor`, `logColor` and `logOut` are written by `Init`/`SetOutput` and read by `logf` outside `logMu`. No live race today because `Init` only runs at startup and tests never use `t.Parallel`, but it is the one concurrency wart in the codebase if someone adds parallel tests.

[User Instruction Summary]
- Date: 2026-10-08
- Context: User reviewing the HTA conversion task ("把本地 mrp.bat 改造为可视化的 hta 代码")
- Instructions:
  - Do not auto-commit code. Only commit, push, or create branches when the user explicitly asks for it.

[User Instruction Summary]
- Date: 2026-10-14
- Context: User reviewing the mrp route debug log ("不行，不够简单直观")
- Instructions:
  - 日志要简单直观：一行只回答一个问题，用自然语句。路由日志写成 `route <入站完整URL> -> <改写后实际发出的URL>`。入站侧写全协议+域名+端口+路径（端口即使为 80/443 也照写，用 JoinHostPort 保持 IPv6 括号）；箭头右侧是真正发出去的那个 URL，不写配置里的 upstream 原值。status 路由写 `status <码>`，本地目录路由写目录路径，未命中写 `passthrough`。不输出 protocol/port/prefix 等中间匹配字段，也不区分"域名无条目"与"前缀未命中"两种 miss。
  - 日志里的出站 URL 必须由转发代码自身产出（`Route.OutboundURL`，被 `rewriteRequest` 与 routeHandler 共用），不得另行重建，否则日志与真实转发会漂移。同理删除只为写日志而生的中间产物（如路由决策结构体），直接复用 `Pick`。
  - 上游错误日志写实际拨号目标：`upstream=<URL>`。`UpstreamErrorHandler` 收到的是已被改写的出站请求，原写法 `domain=request.Host` 恒为空、`path` 是改写后的路径，两个字段都有误导。

[User Instruction Summary]
- Date: 2026-10-15
- Context: User asking to polish the remaining log lines and error messages in mrp ("其它的日志或错误文案再优化下")
- Instructions:
  - 日志/错误文案统一句式：`<主语> <状态词>`，主语在前，状态词用过去分词表示已完成或失败的动作（`config reloaded`、`reload failed`、`mitm started`、`tunnel started`、`shutdown complete`），随后挂 `key=value` 事实。避免 `connect tunnel` / `connect mitm` 这类把名词堆在一起的写法。
  - 降级不要写成失败：没配 CA 导致走不了 MITM 写 `mitm skipped ...: no ca certificate configured`，不写 `connect failed`——CONNECT 本身成功了，失败的是中间人这条选择。
  - 同一个主语的 `key=value` 不要重复出现：`ca certificate not found cert=ca.crt` 去掉第二个 `key=`；`nameservers updated addresses=...` 把 key 从 `nameservers` 改成 `addresses`。
  - key 名贴住它存的东西：配置文件路径写 `config=<path>`，不写 `path=<path>`（后者让人怀疑是哪个路径）；上游地址写 `upstream=`。
  - 丢掉外层流程已经给出的上下文：`listener close failed` 去掉 `during shutdown`；`shutdown complete` 去掉 `all connections drained`。
  - 被 wrap 的错误自身已经说了操作和路径就别再包一层前缀：`tls.LoadX509KeyPair` 直接 `return err`，省掉 `load key pair: %w`。
  - 配置校验错误固定为 `domain %q prefix %q <投诉>`：先主语、再出错的字段值、最后以字段名开头的具体投诉（`must start with /`、`is duplicated`、`status or upstream is required`）。
  - `cmd/mrp/main.go` 里 `flag.Parse()` 是默认 ExitOnError，格式非法的 flag（`--port abc`）由 flag 包自己打印后 os.Exit(2)，`log.Fatal("flag parsing failed: ...")` 只接得住我们自校验的越界值（`--port 0`、`--log trace`）。不要把那段当成格式错误兜底。

[User Instruction Summary]
- Date: 2026-10-15
- Context: User rejecting the `retrying in %s` → `retry_in=%s` rewrite ("retry_in 不合理，排查所有类似问题")
- Instructions:
  - `key=value` 只挂"原子事实"（一个名词加一个值：`port=443`、`elapsed=3ms`）。自然动词短语不能被压成 key——`retrying in 5s` 是谓语，写成 `retry_in=5s` 读起来像配置项。原文 `listener accept failed, retrying in 5s` 是对的，已恢复。排查方式是 grep 出所有 log/Errorf 后逐个看 `=` 左边的词能不能当名词读。
  - 顺手丢了主语也是错：`listener accept failed` 不能缩成 `accept failed`，`accept` 不说清 accept 的是什么。缩主语前先问删掉的那个词有没有信息量。
  - 归一化时不得丢掉带信息的记号。`upstream %q is malformed, use host or [host]:port` 里的方括号是 IPv6 必需的（该函数注释明确支持 `[IPv6]:port`），不能为了好看写成 `host:port`。
  - 语法要过关：`prefix %q is duplicate` 不对，写 `is duplicated`。套句式模板前先检查产出是不是正常英语。
  - 诊断+要求不要两句重复同一件事：`port is invalid, must be 1-65535` 两句都在说范围，`port must be 1-65535` 已完整。同理 `--cert and --key: must be set together` 里的 `set` 多余。
  - 缩写要对齐 Go 惯例：`missing sni` → `sni is missing`（主语在前 + SNI 大写）；`certificate is not a ca` → `ca certificate is not a ca`，因为同一个 `ca` 包既有 CA 证书加载也有 leaf 证书，得说清是哪个。
  - wrap 前缀只在内层错误没说清时才加：`dns.New` 的唯一错误以 `dns nameserver` 开头，`init dns: %w` 重复，直接 `return err`；但 `read config: %w` 包的是 `open <path>: ...`，没说"config"，前缀有用，保留。

[User Instruction Summary]
- Date: 2026-10-15
- Context: User asking to apply the same wording rules to mrp.hta ("同理优化 mrp.hta")
- Instructions:
  - mrp.hta 的用户可见文案与 Go 侧日志同一套句式：`<主语> <状态词>`，状态词用过去分词（`CA removed`、`Proxy not applied`、`Config sync failed`）。级别词绝不写进正文——`Warning: proxy not applied` 的 `Warning:` 由 log level 承担，删掉。
  - 降级写成"做到了什么 + 哪部分没做到"，主语仍是主语：`Binary installed, CA push failed for <files>`，不写 `Binary installed, but push failed for: ...`（`but` 一出现主语就换成失败项了）。
  - 一句内多个事实统一用 ` - ` 连接，不混用 `;` 再接一个以大写开头的整句：`System proxy may still be set - clear it manually.`。
  - 返回值类事实用裸 `key=value`，不加括号：`DevTools forward failed rc=2`、`Binary push failed rc=1`。
  - 两种场合用两种形式：表格单元格是 `key: value`（`Missing: ca.crt`），日志散文是叙述句（`Missing ca.crt in <dir> - download from GitHub Releases.`）。
  - 不写行动指引型废话：`Not probed yet; click Refresh to check.` → `Not probed.`，界面自身已有 Refresh 入口。
  - 启动 banner 用 key=value：`host=mshta workdir=<dir> - click Refresh to probe modules.`，另一分支 `host=browser - no shell available, actions will fail.`。
  - 动作名必须贴住它真正做的事：`Locate files` 实际是补写 CA 文件再核对缺件，改成 `Check files`；改名要同时改 `states` 里引用的 `act` 字符串（`rowAct` 按名字匹配）。
  - 复位状态时把派生字段一起写掉，别靠 getter 里的空列表兜底文案：uninstall 里 `s.miss = [MRP_EXE, CRT, KEY]`，否则 Files 行会渲染成 `Missing: ` 加空串。
  - 改动后自检三件事：UTF-8 无 BOM、JS 字符串字面量全 ASCII、括号配平。渲染路径零非 ASCII（中文只允许出现在 HTML 注释和 `//` 注释里），自检方式是解析出 `<script>` 内所有字符串字面量再扫 ord>127。

[User Instruction Summary]
- Date: 2026-10-10
- Context: User reviewing comment style in the rewritten mrp.hta ("不要在注释中写思考过程和其他一些流水账记录")
- Instructions:
  - 注释只保留两种：简短的"为什么"说明，或一行功能/职责描述。禁止在注释中写思考过程、多行推理叙述（如"否则会……导致……"式推演）。
  - 禁止流水账式记录：改动来源、修复历史、方案取舍过程（如"沿用已验证逻辑""不做占位""Proxy 换 defineProperty""与旧版相比"）。变更背景写在对话回复里，注释只描述当前代码的事实。

[Project Knowledge Summary]
- Date: 2026-10-14
- Context: Discovered by Agent while diagnosing mrp MITM "http: TLS handshake error ... local error: tls: bad record MAC"
- Category: Troubleshooting & Debugging
- Instructions:
  - "bad record MAC" 在 mrp MITM 路径下不是字节管道/会话票证/并发 bug，而是 Go 1.25.x 把 TLS 1.3 客户端"拒绝 MITM 证书"误报：客户端在 server hello 后以明文发 alert（unknown_ca/bad_certificate），Go 服务端 cipher 已装好（setTrafficSecret），把明文 alert 当加密记录 AEAD.Open 失败 → alertBadRecordMAC。对应 Go issue #76767。同一场景 TLS 1.2 正确报 "remote error: tls: unknown certificate authority"。
  - 复现方法：curl 经 mrp 用无关 --cacert 走 TLS 1.3 → 代理日志出现 "local error: tls: bad record MAC"，curl 端显示 "TLS alert, unknown CA (560)"。
  - 真正根因在客户端：拒绝 mrp 现场签发证书——证书固定（pinning）或 App 未信任用户 CA（Android 7+ 须 App 显式允许用户 CA）。mrp 无法绕过 pinning，字节层面无解。
  - 不要再为该错误尝试会话票证/复用类修复：禁用 SessionTickets 无效（已实测并 revert，提交 2c66987→14b228e）。生产路径 mitmTLSConfig 每连接 Clone 会让 MITM 服务端会话票证密钥逐连接随机生成、客户端跨连接复用必然失败（实测 resumed=0），但这是性能问题、不产生 bad record MAC。
  - 若需让固定证书域名仍经 mrp 路由，只能走透传隧道（不 MITM）：当前需把该域移出 servers 配置使其落入 tunnel 路径；命中域目前固定 MITM，无 per-route passthrough 开关。

[Project Knowledge Summary]
- Date: 2026-10-15
- Context: Discovered by Agent while polishing mrp.hta user-facing strings
- Category: Environment Configuration
- Instructions:
  - 本环境没有 JS 运行时：`node`/`deno`/`bun`/`qjs` 全缺，`apt-get install nodejs` 失败（apt 索引为空，报 `Unable to locate package nodejs`）。改 mrp.hta 只能做静态校验，无法实际执行。
  - 静态校验做法：用 Python 写一个状态机扫描 `<script>` 内容，跳过 `//`、`/* */`、字符串字面量、正则字面量（正则判定：`/` 前的最后一个有效字符属于 `(,=:[!&|?{};+-*%^~<>` 或行首），统计 `()`/`[]`/`{}` 是否配平且无负数下溢。注意正则字面量里带单双引号（`/["]/`、`/[&|^<>"'%\s]/`），朴素按行数引号的方法必误报。
  - mrp.hta 渲染文本的 ASCII 自检：解析 `<script>` 内全部字符串字面量（约 492 个）逐个查 ord>127，期望 0 个；`<style>` 外的 HTML 与注释允许有中文（文件头注释、`// ---- 分节 ----`），它们不进入渲染路径。
