# 我的反向代理 (mrp)

Go 实现的本地反向代理。单一端口同时处理 HTTP / HTTPS（按连接首字节自动识别），依据 YAML 路由配置按域名（SNI / Host）与路径前缀转发。用于把移动 App 固定访问的域名劫持转发到自有服务器联调。

## 原理

目标 App 通过 HTTPS 访问固定域名（如 `api.target-app.com`）。mrp 持有一张自签 CA，在 TLS 握手时按客户端 SNI 实时签发对应域名的服务端证书，把 CA 导入设备信任后，将设备流量引导到 mrp——hosts / DNS 重定向让 App 直连 mrp，或把设备代理指向 mrp 的 CONNECT。

核心是 MITM：App 发来带 SNI 的 TLS ClientHello，mrp 用 CA 当场签发一张 SAN 为该 SNI 的服务端证书冒充目标域名完成握手，客户端因信任本地 CA 而验证通过，于是 mrp 拿到解密后的明文 HTTP：

```mermaid
sequenceDiagram
    participant App as App客户端
    participant mrp as mrp代理
    participant Up as 上游服务器

    App->>mrp: TCP连接 + TLS ClientHello
    mrp->>App: 服务端证书握手完成
    App->>mrp: GET /v1/hello 已解密
    mrp->>mrp: "按 SNI(api.target-app.com) 与路径前缀匹配"
    mrp->>Up: 重新建立 HTTPS 连接并转发
    Up-->>mrp: HTTP 响应
    mrp-->>App: 加密回传响应
```

随后按 SNI / Host 与路径前缀匹配路由，转发到真实上游，未匹配的域名透传原目标；上游为 HTTPS 时默认校验其证书，可用 `tls_verify: false` 关闭。纯 HTTP 连接（同一端口按首字节识别）不经 TLS 握手，直接按 Host 头路由；显式代理的 CONNECT 则先返回「200 Connection Established」建立隧道，命中域名再走上述 MITM 流程。

## 使用流程

### 1. 准备 CA 证书和私钥

生成一张自签 CA，mrp 会用它按 SNI 动态签发各域名的服务端证书，因此 CA 本身无需预写拦截域名：

```bash
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -keyout ca.key -out ca.crt -nodes -days 3650 \
  -subj "/CN=DeviceProxy CA" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,digitalSignature"
```

### 2. 导入 CA 证书

把 `ca.crt` 导入客户端设备，使其信任 mrp 签发的证书。

#### Android

- **用户证书**：设置 → 安全 → 更多安全设置 → 加密与凭据 → 安装证书 → CA 证书
- **系统证书（需 Root）**：Android 7.0+ 第三方 App 默认不信任用户证书，需装入系统证书存储

  ```bash
  HASH=$(openssl x509 -subject_hash_old -in ca.crt | head -1)
  adb push ca.crt /data/local/tmp/${HASH}.0
  adb shell "su -c 'mount -o rw,remount /system && \
    cp /data/local/tmp/${HASH}.0 /system/etc/security/cacerts/ && \
    chmod 644 /system/etc/security/cacerts/${HASH}.0'"
  ```

#### Windows

```bash
# 当前用户（免管理员）
certutil -user -addstore Root ca.crt

# 或机器全局（需管理员）
certutil -addstore Root ca.crt
```

#### Linux

```bash
# Debian / Ubuntu：装入系统信任并刷新
sudo cp ca.crt /usr/local/share/ca-certificates/mrp-ca.crt
sudo update-ca-certificates
```

也可以用 `curl --cacert ca.crt` 临时信任，无需系统导入。

### 3. 初始化配置文件

mrp 首次运行会自动在当前目录创建 `routing.yaml`（含注释模板），按需编辑：

```yaml
servers:
  - domain: api.target-app.com
    routes:
      - prefix: /v1/
        upstream: https://our-server-a.com/v1/
      - prefix: /
        upstream: http://192.168.1.50:8080
        host: our-server-b.com
  - domain: cdn.target-app.com
    routes:
      - prefix: /
        upstream: https://our-server-b.com
        tls_verify: false
```

字段说明：

| 字段 | 说明 |
|------|------|
| `servers[].domain` | 按域名精确匹配，HTTPS 用 SNI、HTTP 用 Host 头 |
| `routes[].prefix` | 最长路径前缀匹配 |
| `routes[].upstream` | 上游地址，路径前缀自动映射 |
| `routes[].host` | 可选，改写转发时的 Host 头 |
| `routes[].tls_verify` | 可选，默认 true，false 跳过 HTTPS 上游证书校验 |

未匹配的域名透传原目标。修改 `routing.yaml` 后自动热加载，无需重启。

### 4. 启动服务

```bash
go build -trimpath -ldflags "-s -w" -o mrp .

# 默认读取当前目录 routing.yaml（缺失自动创建）、监听 443、加载 ca.crt / ca.key
./mrp
```

命令行参数：

| 参数 | 默认 | 说明 |
|------|------|------|
| `--config` | `routing.yaml` | 路由配置文件路径；未指定时缺失则自动创建 |
| `--port` | `443` | 监听端口 |
| `--cert` / `--key` | `ca.crt` / `ca.key` | CA 证书/私钥，成对提供；mrp 按客户端 SNI 动态签发服务端证书；缺省时仅支持 HTTP 与 CONNECT 隧道 |
| `--log` | `info` | debug / info / warn / error |

### 5. 测试验证

```bash
# 直连 HTTPS（域名解析到本机，mrp 终结 TLS 后转发）
curl --cacert ca.crt --resolve api.target-app.com:443:127.0.0.1 \
  https://api.target-app.com/v1/hello

# 纯 HTTP（同一端口，自动识别）
curl --resolve api.target-app.com:443:127.0.0.1 \
  http://api.target-app.com:443/v1/hello

# 显式代理（CONNECT）
curl -x http://127.0.0.1:443 --cacert ca.crt \
  https://api.target-app.com/v1/hello
```

观察 mrp 日志（`domain` / `path` / `upstream` / `status` / `elapsed`）确认路由命中与转发结果；上游不可达时返回 502。

### 6. 各平台设备对接代理

代理地址填 mrp 所在机器能被设备访问到的 IP（不能用 `127.0.0.1`），端口即 `--port`（默认 `443`）。

#### Android

```bash
# 设置全局 HTTP 代理（IP 换成 mrp 所在机器的局域网地址）
adb shell settings put global http_proxy 192.168.1.100:443

# 取消代理
adb shell settings put global http_proxy :0
```

部分 App 自实现网络栈、忽略系统代理，这类 App 需改用 hosts / DNS 重定向方式。

#### Windows

```bash
# 设置 WinHTTP 系统代理（服务与部分命令行工具生效）
netsh winhttp set proxy 192.168.1.100:443

# 取消代理
netsh winhttp reset proxy
```

浏览器与多数桌面应用走 WinINET（GUI）：设置 → 网络和 Internet → 代理 → 手动设置代理，填入 `192.168.1.100:443`，关闭时切回「自动检测」。

#### Linux

```bash
# 设置会话级代理（curl 等命令行工具生效）
export http_proxy=http://192.168.1.100:443 https_proxy=http://192.168.1.100:443

# 取消代理
unset http_proxy https_proxy
```