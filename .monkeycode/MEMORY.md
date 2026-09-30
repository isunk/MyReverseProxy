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
  - The per-request logging path is the largest purely-mrp allocation cluster left (`logInfof` = 820 ns, 5 allocs, 392 B; `formatLogLine` = 448 ns, 4 allocs) but is ~1.3% of the 62 µs total proxy overhead, so it is not worth adding a `sync.Pool`.
  - Hardcoded `ResponseHeaderTimeout=30s` and `tunnelDialTimeout=10s` were deliberately left as constants rather than flags: 30 s for response *headers* is already generous for a mobile-network proxy, and disabling it would hang on dead upstreams. The DNS half of the timeout is already tunable via `--dns-timeout`.
  - Second pass (`internal/proxy/proxy_bench_test.go`, full real MITM chain with genuine client-side cert verification): mrp steady state is 69 µs/request at 179 allocs vs 25 µs for an identical upstream reached directly — about 44 µs of mrp overhead, dominated by upstream RTT, not mrp. First-ever request 4.2 ms, repeat cold request 1.4–1.75 ms, one cert signing 166 µs. Tunnel pass-through 290 MB/s on loopback, so the 32 KB `io.Copy` buffer is not worth enlarging.
  - The one real stall measured: `dns.resolve` tries nameservers serially with a 1 s per-server cap, so a first nameserver that silently drops queries costs **1.001 s** on the first request for a new domain (a nameserver that refuses costs 0 ms and switches instantly). Public DNS silently dropped by NAT/firewall is the common shape of this. The user chose to keep the serial order, so the configured order remains a priority semantic and the README wording "按配置顺序故障切换" stays correct. Do not convert `resolve` to concurrent racing.
  - `NewTransport` builds on `http.DefaultTransport.Clone()`, which silently inherits `ExpectContinueTimeout=1s`, `IdleConnTimeout=90s`, `TLSHandshakeTimeout=10s` and `ForceAttemptHTTP2=true`. Left as-is: the 100-continue wait was never reproducible on loopback (a Go handler reading the request body auto-sends 100 Continue), so `ExpectContinueTimeout=0` has no measured benefit.
