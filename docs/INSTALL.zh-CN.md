[English](INSTALL.md) | [简体中文](INSTALL.zh-CN.md)

# 安装

每个正式 Tag 会发布两个命令：

- `llm-gateway`：数据面进程；
- `llm-gateway-console`：可选控制面进程。

Release 压缩包支持 Linux 和 macOS 的 amd64/arm64，以及 Windows amd64。
仓库还会发布 Linux amd64/arm64 容器镜像与 Helm Chart 压缩包。运行时依赖
MySQL 8.0+ 与 Redis 7+；当 `usage_events.driver=kafka` 时还需要 Kafka。

## Release 压缩包

从 [GitHub Release](https://github.com/Zereker/llm-gateway/releases) 下载对应平台的
压缩包与 `SHA256SUMS`，解压前先校验：

```sh
# Linux
sha256sum --check --ignore-missing SHA256SUMS

# macOS（将文件名替换为实际下载的压缩包）
grep 'llm-gateway_v0.1.0_darwin_arm64.tar.gz' SHA256SUMS | shasum -a 256 -c -
```

每个压缩包都包含两个命令、生产配置模板、安装说明与许可证。先确认二进制内嵌的
构建信息：

```sh
./llm-gateway -version
./llm-gateway-console -version
```

将二进制安装到 `PATH`，把 `configs/` 中的模板复制到
`/etc/llm-gateway/`，再通过文档声明的环境变量注入 Secret。Gateway 至少需要
`LLM_GATEWAY_DATA_KEY`，提供的生产 Gateway 模板中包含数据库连接配置。Console
还需要 `LLM_GATEWAY_DATABASE_DSN`、与 Gateway 相同的 `LLM_GATEWAY_DATA_KEY`
以及 `LLM_GATEWAY_CONSOLE_TOKENS`。

```sh
install -m 0755 llm-gateway llm-gateway-console /usr/local/bin/
install -d -m 0750 /etc/llm-gateway
install -m 0640 configs/gateway.yaml configs/console.yaml /etc/llm-gateway/

export LLM_GATEWAY_DATA_KEY="$(openssl rand -hex 32)"
llm-gateway -config /etc/llm-gateway/gateway.yaml
```

Gateway 与 Console 不能分别生成不同的 data key。若不先重新加密数据库中的
Endpoint 凭据就更换 key，这些凭据将无法解密。

## 容器镜像

正式 Tag 会发布两个独立镜像：

```sh
docker pull ghcr.io/zereker/llm-gateway:v0.1.0
docker pull ghcr.io/zereker/llm-gateway-console:v0.1.0
```

两个镜像都以 UID/GID `65532:65532` 运行。请只读挂载对应配置，并通过编排系统
提供 Secret。项目不发布 `latest` Tag；请使用不可变版本 Tag 或 Digest。

## Helm

GitHub Release 中包含 `llm-gateway-0.1.0.tgz`。Chart 只部署 Gateway 数据面；
MySQL、Redis、Kafka 和可选 Console 都是外部依赖。

```sh
helm install ai-gw ./llm-gateway-0.1.0.tgz \
  --set-string secrets.databaseDSN='user:pwd@tcp(mysql:3306)/llm_gateway?parseTime=true&charset=utf8mb4' \
  --set-string secrets.dataKey="$(openssl rand -hex 32)"
```

生产环境请使用 `secrets.existingSecret`，不要把 Secret 放在命令行参数中。详见
Chart 的[部署指南](../deploy/helm/llm-gateway/README.md)。

## 从源码构建

构建需要 `go.mod` 声明的 Go 版本：

```sh
git clone https://github.com/Zereker/llm-gateway.git
cd llm-gateway
git checkout v0.1.0
make build VERSION=v0.1.0
./bin/llm-gateway -version
```

在默认 Go Module Proxy 或容器 Registry 访问较慢的地区，所有构建入口都支持
指定镜像，不强制依赖单一官方地址：

```sh
GOPROXY=https://goproxy.cn,direct make build VERSION=v0.1.0

BASE_IMAGE_REGISTRY=docker.1ms.run \
GOPROXY=https://goproxy.cn,direct \
make docker-build VERSION=v0.1.0
```

Quickstart 还支持通过 `CONTAINER_REGISTRY`、`BASE_IMAGE_REGISTRY` 和
`GOPROXY` 环境变量选择镜像。

## 升级边界

升级前先阅读[变更日志](CHANGELOG.zh-CN.md)。从 `v0.1.0` 开始，已合入的数据库
Migration 文件不可修改；升级只能增加新的编号 Migration。变更生产部署前，必须
备份 MySQL，并妥善保留 Endpoint 凭据所使用的 data key。
