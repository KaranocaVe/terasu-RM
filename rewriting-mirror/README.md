# Rewriting Mirror

Rewriting Mirror 是一个本地反向代理，用于把上游 URL 重写为本地 HTTP 入口，
让 Docker/Podman 客户端在不做 TLS MITM 的前提下拉取镜像或其他资源。
它使用 TeRaSu 的 TLS ClientHello 分片和 DoH/DoT DNS 来提高直连成功率，
同时保持端到端 TLS 完整性。

## 为什么不是普通 HTTP 代理？

普通 HTTP 代理要改写 HTTPS 流量，必须做 TLS 终止（MITM），这要求客户端安装自签证书。
Rewriting Mirror 通过本地 HTTP 入口重写上游的绝对 URL（例如 Location、WWW-Authenticate），
避免了证书部署问题。

## 主要特性

- 基于路径的多上游路由。
- 自动重写 Location 和 WWW-Authenticate 头。
- 对大文件/模型的并发流式转发。
- 自定义传输层，带 TeRaSu 分片和 DoH/DoT DNS。
- 连接 reset 时自动回退到更小的 TLS 分片长度。
- 可选的并发 inflight 限流（支持等待或直接拒绝）。
- Prometheus 指标 `/metrics`。
- JSON 结构化访问日志和错误日志。
- 健康检查与就绪检查端点。
- SIGHUP 热加载配置与启动自检。

## 构建

需要 Go 1.22+。

```
GOPROXY=https://goproxy.cn,direct go build ./cmd/rmirror
```

## 运行

```
./rmirror -config examples/docker.json
```

## CLI 参数

```
-rmirror -config <path>
-rmirror -validate
-rmirror -print-default-config
-rmirror -version
-rmirror -check-upstreams
```

## 示例配置

- Docker Hub: `examples/docker.json`
- GitHub: `examples/github.json`
- Hugging Face: `examples/huggingface.json`

## 配置说明（JSON）

顶层字段：

- `listen`: 监听地址，默认 `127.0.0.1:5000`。
- `public_base_url`: 可选，用于重写时的绝对 URL 基址。
- `access_log`: 是否启用访问日志。
- `tls`: 可选，TLS 监听配置。
- `timeouts`: 服务端超时配置。
- `transport`: 上游传输配置。
- `limits`: 并发限制。
- `routes`: 路由表。

`tls`：

- `cert_file`: TLS 证书文件路径。
- `key_file`: TLS 私钥文件路径。

`timeouts`：

- `read_header_timeout`, `read_timeout`, `write_timeout`, `idle_timeout`,
  `shutdown_timeout`: Go duration 字符串。
- `max_header_bytes`: 最大请求头大小。

`transport`：

- `first_fragment_len`: TLS ClientHello 首分片长度（0 表示禁用分片）。
- `dial_timeout`, `keepalive`, `idle_conn_timeout`, `tls_handshake_timeout`,
  `response_header_timeout`, `expect_continue_timeout`: Go duration 字符串。
- `max_idle_conns`, `max_idle_conns_per_host`, `max_conns_per_host`。
- `force_http2`, `disable_compression`。

`limits`：

- `max_inflight`: 最大并发请求数（0 表示不限制）。
- `max_inflight_wait`: 超过并发上限后的等待时间（到期后返回 429）。

`routes` 条目：

- `name`: 日志可读名称。
- `public_prefix`: 对外路径前缀。
- `upstream`: 上游基地址或主机名。
- `preserve_host`: 是否保留原 Host 头。

Schema 文件：`config.schema.json`。

## 健康检查

- `/_rmirror/healthz` 返回 `200 ok`。
- `/_rmirror/readyz` 在未过载时返回 `200 ok`。

## 指标

- `GET /metrics` 提供 Prometheus 指标。
- 包含请求次数、延迟、字节数、inflight gauge、TLS 回退计数等。

## 日志

访问日志与错误日志均为 JSON 行，示例：

```
{"ts":"2026-01-22T16:43:23.123Z","level":"info","msg":"request","method":"GET","path":"/v2/...","status":200,"bytes":1234,"duration":42,"route":"docker-registry","upstream":"registry-1.docker.io"}
```

## 上游自检

启动时可使用 `-check-upstreams` 检查所有上游。自检会先发 `HEAD`，
若失败或被拒绝，再使用 `GET` + `Range: bytes=0-0`。状态码 < 500 即视为可达。

## 配置热加载

对进程发送 `SIGHUP` 可热加载 routes、transport、limits。
监听地址、TLS 配置和服务端超时需要重启。

示例：

```
kill -HUP $(pidof rmirror)
```

## Docker 守护进程配置

本地 HTTP 镜像源：

```
{
  "registry-mirrors": ["http://127.0.0.1:5000"],
  "insecure-registries": ["127.0.0.1:5000"]
}
```

## Podman 配置

编辑 `/etc/containers/registries.conf`：

```
[[registry]]
location = "docker.io"
[[registry.mirror]]
location = "127.0.0.1:5000"
insecure = true
```

## Systemd 示例

```
[Unit]
Description=Rewriting Mirror
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/rmirror -config /etc/rmirror/config.json
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
```

## 备注

- `public_base_url` 可选；未设置时使用请求的 scheme/host 进行重写。
- 为了让重写后的 URL 仍然走镜像入口，建议把鉴权/CDN 等域名也配置为路由。
- 如果出现 `connection reset by peer`，可以降低 `first_fragment_len`。
  镜像会在安全可重试的请求上自动回退到 1 再到 0。
