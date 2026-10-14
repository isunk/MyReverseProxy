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
  - `route.Table.Pick` stays a linear scan, and `server.ServeSingleConn` keeps allocating a per-connection `*http.Server`: hijack detection needs a per-connection `ConnState`, and sharing one server would race across connections. Both were reviewed and deliberately left.
  - Latent data race in the `log` package: `logFloor`, `logColor` and `logOut` are written by `Init`/`SetOutput` and read by `logf` outside `logMu`. No live race today because `Init` only runs at startup and tests never use `t.Parallel`, but it is the one concurrency wart in the codebase if someone adds parallel tests.

[User Instruction Summary]
- Date: 2026-10-08
- Context: User reviewing the HTA conversion task ("把本地 mrp.bat 改造为可视化的 hta 代码")
- Instructions:
  - Do not auto-commit code. Only commit, push, or create branches when the user explicitly asks for it.

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
