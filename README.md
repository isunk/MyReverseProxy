# 我的反向代理 (mrp)

基于 Go 的单文件反向代理服务。在单一端口上监听，按首个字节自动识别 HTTP / TLS，依据 YAML 路由配置，按请求的域名（SNI / Host）与路径前缀匹配自定义规则，将请求转发到不同的服务器。适用于把移动 App 固定访问的域名劫持转发到自有服务器进行联调。

需求与设计文档见 `.monkeycode/specs/mrp/`。

## 快速开始

```bash
# 本机构建
go build -trimpath -ldflags "-s -w" -o mrp .

# 交叉编译（示例：Android arm64）
GOOS=android GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o mrp-android-arm64 .

# 启动服务（单一端口，自动识别 HTTP / TLS）
./mrp --config routing.yaml --listen :443 --tls-cert certs/server.crt --tls-key certs/server.key
```

支持平台：`android/arm64`、`linux/arm64`、`windows/amd64` 可 `CGO_ENABLED=0` 交叉编译纯静态二进制；`ios/arm64` 需 Apple SDK 与 cgo 链接（以 `-buildmode=c-archive` 嵌入应用），不支持纯静态二进制。项目结构：源码按职责拆分为根目录 `main.go` / `config.go` / `route.go` / `proxy.go` / `server.go`，测试位于 `main_test.go`，命名规范见 `AGENTS.md`。

## 证书创建

程序启动时从配置指定的路径加载证书，证书需要自行通过 openssl 创建。HTTPS 终结需要两份材料：CA 证书（导入客户端设备用于信任）与服务端证书（SAN 覆盖全部目标域名）。

```bash
# 创建输出证书的目录
mkdir -p certs

# 生成 CA 私钥与自签名 CA 证书（有效期 10 年）
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -keyout certs/ca.key -out certs/ca.crt -nodes -days 3650 \
  -subj "/CN=DeviceProxy CA"

# 生成服务端私钥与证书签发请求
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -keyout certs/server.key -out certs/server.csr -nodes \
  -subj "/CN=api.target-app.com"

# 写入 SAN 扩展配置，域名列表按实际目标域名修改
printf "subjectAltName=DNS:api.target-app.com,DNS:cdn.target-app.com\n" > certs/san.cnf

# 用 CA 签发服务端证书（有效期 825 天，满足 iOS 上限要求）
openssl x509 -req -in certs/server.csr \
  -CA certs/ca.crt -CAkey certs/ca.key -CAcreateserial \
  -out certs/server.crt -days 825 -extfile certs/san.cnf

# 校验 SAN 是否覆盖全部目标域名
openssl x509 -in certs/server.crt -noout -text | grep -A1 "Subject Alternative Name"
```

## 证书导入设备

### Android（用户证书）

1. 将 `certs/ca.crt` 复制到设备存储
2. 进入 设置 → 安全 → 更多安全设置 → 加密与凭据 → 安装证书 → CA 证书
3. 选择 `ca.crt` 完成安装

### Android（系统证书，需 Root）

Android 7.0 以上第三方 App 默认不信任用户证书，联调第三方 App 需将 CA 装入系统证书存储：

```bash
# 计算 Android 证书存储要求的哈希文件名
HASH=$(openssl x509 -subject_hash_old -in certs/ca.crt | head -1)

# 推送证书到设备临时目录
adb push certs/ca.crt /data/local/tmp/${HASH}.0

# 以 root 写入系统证书目录（也可使用 Magisk 模块方式固化）
adb shell "su -c 'mount -o rw,remount /system && cp /data/local/tmp/${HASH}.0 /system/etc/security/cacerts/ && chmod 644 /system/etc/security/cacerts/${HASH}.0 && mount -o ro,remount /system'"
```

### iOS

1. 通过隔空投送、邮件或文件 App 将 `certs/ca.crt` 发送到设备
2. 进入 设置 → 已下载的描述文件，点击安装
3. 进入 设置 → 通用 → 关于本机 → 证书信任设置，开启对该 CA 的完全信任

### HarmonyOS

1. 将 `certs/ca.crt` 复制到设备存储
2. 进入 设置 → 安全 → 更多安全设置 → 加密和凭据 → 安装证书 → CA 证书
3. 选择 `ca.crt` 完成安装

### Windows

```bash
# 导入当前用户受信任的根证书存储（免管理员）
certutil -user -addstore Root certs\ca.crt
```

导入机器全局存储（需管理员）：`certutil -addstore Root certs\ca.crt`。

## 配置示例

配置文件只承载路由规则，监听地址与证书路径通过命令行参数指定。

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

规则说明：

- `servers[].domain`：按域名精确匹配，HTTPS 依据 SNI、HTTP 依据 Host 头识别域名
- `routes[].prefix`：最长路径前缀匹配
- `routes[].upstream`：上游地址，路径前缀自动映射
- `routes[].host`：可选，改写转发时的 Host 头
- `routes[].tls_verify`：可选，默认 true，设为 false 跳过 HTTPS 上游证书校验
- 未匹配的域名直接透传原始目标

## 流量引导

服务只负责转发到达端口的请求，客户端流量通过以下任一方式引导：

- 设备 hosts / DNS：把目标域名解析指向本服务所在机器 IP
- 显式代理：客户端代理设置指向本服务地址与端口（支持 CONNECT）
