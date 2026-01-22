# TeRaSu Rewriting Mirror

[![ci](https://github.com/KaranocaVe/terasu-RM/actions/workflows/ci.yml/badge.svg)](https://github.com/KaranocaVe/terasu-RM/actions/workflows/ci.yml)
[![release](https://github.com/KaranocaVe/terasu-RM/actions/workflows/release.yml/badge.svg)](https://github.com/KaranocaVe/terasu-RM/actions/workflows/release.yml)
[![release-version](https://img.shields.io/github/v/release/KaranocaVe/terasu-RM?include_prereleases&sort=semver)](https://github.com/KaranocaVe/terasu-RM/releases)
[![license](https://img.shields.io/github/license/KaranocaVe/terasu-RM)](LICENSE)

TeRaSu Rewriting Mirror 是一个本地加速镜像：在本机提供自加速镜像，
用来在中国大陆网络下加速 Docker 镜像、GitHub 代码仓库和 Hugging Face 模型等资源的拉取。

## 快速开始（单实例）

构建二进制：

```
GOPROXY=https://goproxy.cn,direct go build ./cmd/rmirror
```

启动 Docker 镜像源加速：

```
./rmirror -config examples/docker.json
```

配置 Docker 使用本地镜像源（`/etc/docker/daemon.json`）：

```
{
  "registry-mirrors": ["http://127.0.0.1:5000"],
  "insecure-registries": ["127.0.0.1:5000"]
}
```

重载 Docker 后测试：

```
docker pull hello-world:latest
```

## 快速开始（多实例，推荐）

Docker 需要占用根路径 `/`，会与其他业务路由冲突。
要同时加速 Docker、GitHub、Hugging Face，建议多实例多端口。

构建守护进程：

```
GOPROXY=https://goproxy.cn,direct go build ./cmd/rmirrord
```

启动守护进程（会拉起多个 rmirror 实例）：

```
./rmirrord -config examples/daemon.json
```

确保 `rmirror` 二进制与 `rmirrord` 同目录，或在 `PATH` 中可见。

`examples/daemon.json` 中的路径相对该文件所在目录。
默认端口如下：

- Docker: `127.0.0.1:5000`
- Hugging Face: `127.0.0.1:5001`
- GitHub: `127.0.0.1:5002`

## GitHub 使用方法（git clone/pull）

启动 GitHub 实例：

```
./rmirror -config examples/github.json
```

克隆示例仓库：

```
git clone http://127.0.0.1:5002/octocat/Hello-World.git
```

## Hugging Face 使用方法

启动 Hugging Face 实例：

```
./rmirror -config examples/huggingface.json
```

获取模型文件示例：

```
curl -O http://127.0.0.1:5001/gpt2/resolve/main/config.json
```

## Podman 使用方法

编辑 `/etc/containers/registries.conf`：

```
[[registry]]
location = "docker.io"
[[registry.mirror]]
location = "127.0.0.1:5000"
insecure = true
```

## 常用参数

rmirror：

```
-config <path>
-validate
-print-default-config
-version
-check-upstreams
```

rmirrord：

```
-config <path>
-validate
-print-default-config
-version
```

## 热加载与自检

- rmirror 支持 `SIGHUP` 热加载（routes/transport/limits）。
- rmirrord 支持 `SIGHUP` 重新拉起/重载实例配置。
- `-check-upstreams` 会在启动时对上游做 HEAD/Range 检查。

## 监控与健康检查

- `/metrics`：Prometheus 指标。
- `/_rmirror/healthz`：健康检查。
- `/_rmirror/readyz`：就绪检查（过载时返回非 200）。

## 配置文件要点（rmirror）

完整结构见 `config.schema.json`。常用字段：

- `listen`：监听地址。
- `routes`：路由表（`public_prefix` + `upstream`）。
- `transport.first_fragment_len`：TLS ClientHello 首分片长度。
- `limits.max_inflight`：并发限制。
- `access_log`：访问日志开关。

## 配置文件要点（rmirrord）

使用 `examples/daemon.json` 作为模板：

- `instances[].name`：实例名。
- `instances[].config`：对应 rmirror 配置路径（相对 daemon 配置文件所在目录）。
- `restart`：统一重启策略，可被实例覆盖。

## Systemd 示例（可选）

```
[Unit]
Description=TeRaSu Rewriting Mirror
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/rmirrord -config /etc/rmirror/daemon.json
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
```

## 自豪地使用

github.com/fumiama/terasu
