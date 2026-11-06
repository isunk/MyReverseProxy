# My Reverse Proxy

作为一个客户端侧部署的代理服务，同时支持对 HTTP/HTTPS 请求的自定义反向代理。

## 功能

- **自定义反向代理**：匹配规则支持域名、端口与路径前缀，可改写 Host 与请求/响应头，支持短路响应与静态文件托管
- **HTTPS 解密**：自签 CA 按目标域名动态签发证书，命中域名的 HTTPS 流量解密后路由；其余域名保持端到端加密，内容不可见
- **DNS 代理解析**：上游域名解析支持指定 DNS 服务器，按序故障切换
- **配置热加载**：路由与 DNS 配置修改即时生效
- **多端部署**：覆盖 Android / HarmonyOS / Windows / Linux，PC 可借道设备网络出口

## 原理

代理监听单一端口，同时接收明文 HTTP 与 HTTPS 流量。请求进入后，代理从请求内容解析目标域名与协议，匹配 `servers` 中对应的 server（protocol 与 port 为可选匹配条件，省略匹配任意），再按路径前缀选择路由。

- **HTTP**：绝对形式请求取 URL 主机、源形式请求取 Host 头作为目标域名，按域名与路径前缀匹配路由，转发至配置的上游
- **HTTPS**：客户端以 CONNECT 请求进入，CONNECT 目标即请求域名，按域名是否命中配置分为两种模式：
    - **MITM（命中域名）**：客户端发来带 SNI 的 TLS ClientHello，代理用自签 CA 现场签发 SAN 为该 SNI 的证书代替目标完成握手，客户端因信任导入的 `ca.crt` 校验通过；代理得到明文 HTTP 后按域名与路径前缀匹配路由，解析上游域名后转发，可改写 Host 与消息头。CA 无需预登记拦截域名，新增域名即签即用；未提供 CA 时命中域名的 CONNECT 返回 502
    - **tunnel（未命中域名）**：代理与目标建立 TCP 连接并回 200 后，客户端与目标端到端协商 TLS，证书链逐字节原样，代理只搬运字节，内容不可见，无需 CA 参与

配置即开关：`servers` 登记域名即对该域名 MITM，删掉回落 tunnel，热加载即时生效；留空即全量 tunnel。

```mermaid
sequenceDiagram
    participant App as App
    participant mrp as mrp
    participant DNS as DNS 服务器
    participant Up as 配置的上游
    participant Tgt as 原始目标

    alt MITM：命中配置域名
        App->>mrp: "CONNECT api.target-app.com:443"
        mrp->>App: "200 Connection Established"
        App->>mrp: TLS 握手（SNI: api.target-app.com）
        mrp->>mrp: 自签 CA 现场签发该域名证书
        mrp-->>App: TLS 握手完成
        App->>mrp: 明文 HTTP 请求
        mrp->>mrp: 按域名与路径前缀匹配路由
        mrp->>DNS: 解析上游域名（结果缓存）
        DNS-->>mrp: 上游地址
        mrp->>Up: 转发请求（可改写 Host 与消息头）
        Up-->>mrp: HTTP 响应
        mrp-->>App: 加密回传响应
    else tunnel：未命中配置域名
        App->>mrp: "CONNECT www.example.com:443"
        mrp->>Tgt: 建立 TCP 连接（失败回 502）
        mrp->>App: "200 Connection Established"
        Note over App,Tgt: 客户端与目标端到端 TLS，证书链保持原始
        App->>mrp: 加密流量
        mrp->>Tgt: 原样搬运字节
        Tgt-->>mrp: 字节流回传
        mrp-->>App: 原样回传
    else 明文 HTTP
        App->>mrp: "GET http://api.target-app.com/v1/hello"
        mrp->>mrp: 按 Host 头匹配路由
        mrp->>DNS: 解析上游域名
        DNS-->>mrp: 上游地址
        mrp->>Up: 转发请求
        Up-->>mrp: HTTP 响应
        mrp-->>App: 原样返回
    end
```

## 使用流程

### 1. 生成并导入 CA

仅 MITM 需要，纯 tunnel 可跳过。

```bash
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -keyout ca.key -out ca.crt -nodes -days 3650 \
  -subj "/CN=DeviceProxy CA" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,digitalSignature"
```

- **Android**：设置 → 加密与凭据 → 安装 CA 证书。Android 7.0+ 应用默认只信系统证书，需应用侧 `network_security_config` 的 `trust-anchors` 加入 `user`；Root 设备可装为系统证书：

  ```bash
  HASH=$(openssl x509 -subject_hash_old -in ca.crt | head -1)

  # adbd 以 root 运行并重新挂载 /system 为可写
  adb root && adb remount
  adb push ca.crt /system/etc/security/cacerts/${HASH}.0
  adb shell chmod 644 /system/etc/security/cacerts/${HASH}.0
  ```

- **HarmonyOS**：开发者模式（或 Root）推送系统证书目录：

  ```bash
  HASH=$(openssl x509 -subject_hash_old -in ca.crt | head -1)
  cp ca.crt ${HASH}.0
  hdc file send ${HASH}.0 /etc/security/certificates
  ```

- **Windows**：`certutil -user -addstore Root ca.crt`
- **Linux**：`sudo cp ca.crt /usr/local/share/ca-certificates/mrp-ca.crt && sudo update-ca-certificates`，临时用 `curl --cacert ca.crt`

### 2. 编写配置

首次运行自动创建 `mrp.yaml`（含注释模板）。

```yaml
# 上游域名解析用 DNS；删除本字段按系统默认解析
nameservers:
  - "114.114.114.114"
  - "8.8.8.8"
# protocol / port 只参与虚拟服务器匹配，省略匹配任意；mrp 只监听 --port
servers:
  - domain: api.target-app.com
    routes:
      - prefix: /v1/
        upstream: https://api.our-server.com/v1/
        host: api.our-server.com
        headers:
          request:
            Authorization: "Bearer token"
          response:
            Access-Control-Allow-Origin: "*"
      - prefix: /assets/
        upstream: ./dist
      - prefix: /
        upstream: http://192.168.1.50:8080
  - domain: admin.target-app.com
    routes:
      - prefix: /health
        status: 200
        body: OK
      - prefix: /
        upstream: http://127.0.0.1:9000
```

- `servers[].domain`：按域名匹配，HTTPS 取 CONNECT 目标主机、HTTP 取 Host 头
- `servers[].protocol` / `port`：可选，限定协议与目标端口，省略匹配任意
- `routes[].prefix`：最长路径前缀匹配，以 `/` 开头
- `routes[].upstream`：上游地址或本地目录（静态托管，目录命中回退 `index.html`）；与 `status` 互斥
- `routes[].status`：200-599 短路响应，204/304 不得带 `body`
- `routes[].body`：响应体，默认 `Content-Type: text/plain; charset=utf-8`
- `routes[].host`：改写转发时的 Host 头
- `routes[].headers.request` / `.response`：覆盖发往上游的请求头 / 下游返回的响应头
- `nameservers`：DNS 列表（裸 IP 或 `host:port`），按序故障切换；省略用系统 DNS

设备上系统 DNS 常不可用，应保留显式 `nameservers`。配置修改自动热加载（轮询变更或 `SIGHUP`）：路由或 `nameservers` 变化时重建转发并清空 DNS 与已签发证书缓存；CA 仅启动时加载一次。

### 3. 启动

从 [Releases](https://github.com/isunk/MyReverseProxy/releases) 下载对应平台二进制（`latest` 为预发布包，`v*` 为正式版），在 `--port` 指定端口监听，加载工作目录下的 `ca.crt` / `ca.key`：

```bash
# Linux / HarmonyOS / Android
./mrp-linux-arm64

# Windows
mrp-windows-amd64.exe
```

参数：

- `--port`（默认 `8000`）：唯一监听端口，须与客户端代理端口一致
- `--config`（默认 `mrp.yaml`）：路由配置文件，缺失自动创建
- `--cert` / `--key`（默认 `ca.crt` / `ca.key`）：CA 证书与私钥，缺省时无法 MITM
- `--log`（默认 `info`）：debug / info / warn / error
- `--dns-timeout` / `--dns-ttl`（默认 `1s` / `30s`）：单次 DNS 解析超时、解析结果缓存时长（`0` 关闭缓存）

Windows 也可双击 `mrp.hta` 部署面板：内置固定 CA 免 openssl，提供 CA 导入、进程启停、系统代理设置，及 Android(adb) / HarmonyOS(hdc) 设备的安装、启动、配置同步与卸载，设备工作目录 `/data/local/mrp`。

### 4. 验证

```bash
# 显式代理：命中配置域名则 MITM 后转发
curl -x http://127.0.0.1:8000 --cacert ca.crt https://api.target-app.com/v1/hello

# 显式代理：未命中则透传隧道
curl -x http://127.0.0.1:8000 https://www.example.com/

# 直连：域名解析到监听端口，按 Host 头路由
curl --resolve api.target-app.com:8000:127.0.0.1 http://api.target-app.com:8000/v1/hello
```

日志打印 `domain` / `protocol` / `port` / `path` / `status` / `elapsed`，CONNECT 标记 `mitm started` / `tunnel started`；上游不可达返回 502。

### 5. 设备对接代理

mrp 运行在设备本机（代理填 `127.0.0.1`）或 PC（填 PC 局域网 IP），端口须与代理一致。

**Android：**

```bash
adb shell mkdir -p /data/local/mrp
adb push mrp-linux-arm64 ca.key ca.crt mrp.yaml /data/local/mrp/
adb shell chmod +x /data/local/mrp/mrp-linux-arm64

# 前台运行输出日志
adb shell "cd /data/local/mrp && ./mrp-linux-arm64"

# 全局代理指向本机，取消改 :0
adb shell settings put global http_proxy 127.0.0.1:8000
```

**HarmonyOS：**

```bash
hdc shell mkdir -p /data/local/mrp
hdc file send mrp-linux-arm64 ca.key ca.crt mrp.yaml /data/local/mrp/
hdc shell "chmod +x /data/local/mrp/mrp-linux-arm64 && cd /data/local/mrp && ./mrp-linux-arm64"

# 全局代理指向本机，取消改 0
hdc shell network-cfg set http_proxy 127.0.0.1:8000
```

Windows 系统代理：`netsh winhttp set proxy 192.168.1.100:8000`（取消 `reset proxy`），浏览器走 WinINET（设置 → 网络和 Internet → 代理）；Linux 会话代理：`export http_proxy=http://192.168.1.100:8000 https_proxy=http://192.168.1.100:8000`。

### 6. PC 借道设备网络出口

mrp 运行在设备上，PC 建立端口转发后将代理指向 `127.0.0.1:<端口>`，流量改从设备出口发出，上游 DNS 解析也在设备侧完成。

```bash
# Android（HarmonyOS 用 hdc fport tcp:8000 tcp:8000）
adb forward tcp:8000 tcp:8000
```

- 留空 `servers` 即全量 tunnel，PC 侧无需导入 CA
- 转发随设备连接存活，断开或重连后需重新执行
- 无视系统代理的应用需 Proxifier 类工具强制接管，改 hosts / DNS 无法改变其流量出口

## 从源码构建

Go 1.25+：

```bash
go build -o mrp ./cmd/mrp

# 交叉编译静态二进制
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o mrp-linux-arm64 ./cmd/mrp
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o mrp-windows-amd64.exe ./cmd/mrp

go test ./...
go test -race ./internal/ca/ ./internal/proxy/
```

## 项目结构

```
cmd/mrp/        入口：命令行解析、信号处理、构建代理实例
internal/
  config/       YAML 配置结构与解析
  route/        路由表、前缀匹配、路径改写（含编码保真）
  proxy/        反向代理核心：请求转发、CONNECT 隧道、MITM 编排、热加载
  frontend/     单端口明文监听、单连接复用（MITM 内层服务）
  ca/           自签 CA 按名字现场签发证书（域名 SAN / IP SAN），并发合并
  dns/          上游 DNS 解析与故障切换、结果缓存
  cache/        通用并发缓存
  log/          控制台日志（级别过滤、按级别着色）
  testutil/     跨包共享测试替身
```
