# 我的反向代理 (mrp)

Go 实现的本地反向代理。单一端口上同时处理 HTTP / HTTPS（按连接首字节自动识别），依据 YAML 路由配置按域名（SNI / Host）与路径前缀转发。用于把移动 App 固定访问的域名劫持转发到自有服务器联调。

## 快速开始

```bash
# 构建
go build -trimpath -ldflags "-s -w" -o mrp .

# 运行（HTTPS 直连 MITM 需证书）
./mrp --config routing.yaml --listen :443 --tls-cert certs/server.crt --tls-key certs/server.key
```

修改 `routing.yaml` 后自动热生效，无需重启或发信号。

## 配置

命令行参数：

| 参数 | 默认 | 说明 |
|------|------|------|
| `--config` | `routing.yaml` | 路由配置文件路径 |
| `--listen` | `:443` | 监听地址 |
| `--tls-cert` / `--tls-key` | — | TLS 证书/私钥，成对提供；缺省时仅支持 HTTP 与 CONNECT 隧道 |
| `--log-level` | `info` | debug / info / warn / error |

路由配置 `routing.yaml`：

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

- `domain`：按域名精确匹配，HTTPS 用 SNI、HTTP 用 Host 头
- `prefix`：最长路径前缀匹配
- `upstream`：上游地址，路径前缀自动映射
- `host`：可选，改写转发时的 Host 头
- `tls_verify`：可选，默认 true，设为 false 跳过 HTTPS 上游证书校验
- 未匹配的域名透传原始目标

## 证书

HTTPS 直连 MITM 需两份材料：CA（导入设备信任）与服务端证书（SAN 覆盖全部目标域名）。

```bash
mkdir -p certs
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -keyout certs/ca.key -out certs/ca.crt -nodes -days 3650 -subj "/CN=DeviceProxy CA"
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -keyout certs/server.key -out certs/server.csr -nodes -subj "/CN=api.target-app.com"
printf "subjectAltName=DNS:api.target-app.com,DNS:cdn.target-app.com\n" > certs/san.cnf
openssl x509 -req -in certs/server.csr -CA certs/ca.crt -CAkey certs/ca.key -CAcreateserial \
  -out certs/server.crt -days 825 -extfile certs/san.cnf
```

设备导入 CA：

- **Android（用户证书）**：设置 → 安全 → 加密与凭据 → 安装证书 → CA 证书
- **Android（系统证书，需 Root）**：7.0+ 第三方 App 默认不信任用户证书，需把 CA 装入系统证书存储

  ```bash
  HASH=$(openssl x509 -subject_hash_old -in certs/ca.crt | head -1)
  adb push certs/ca.crt /data/local/tmp/${HASH}.0
  adb shell "su -c 'mount -o rw,remount /system && cp /data/local/tmp/${HASH}.0 /system/etc/security/cacerts/ && chmod 644 /system/etc/security/cacerts/${HASH}.0'"
  ```

- **iOS**：设置 → 已下载的描述文件 → 安装；再到 通用 → 关于本机 → 证书信任设置 开启完全信任
- **HarmonyOS**：设置 → 安全 → 加密和凭据 → 安装证书 → CA 证书
- **Windows**：`certutil -user -addstore Root certs\ca.crt`

## 流量引导

服务只处理到达端口的请求，客户端流量经以下任一方式引导到本服务：

- 设备 hosts / DNS：把目标域名解析指向本服务所在机器 IP
- 显式代理：客户端代理设置指向本服务地址与端口（支持 CONNECT）