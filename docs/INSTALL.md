[English](INSTALL.md) | [简体中文](INSTALL.zh-CN.md)

# Installation

Tagged releases publish two commands:

- `llm-gateway`: the data-plane process;
- `llm-gateway-console`: the optional control-plane process.

Release archives support Linux and macOS on amd64/arm64, plus Windows amd64.
The repository also publishes Linux amd64/arm64 container images and a Helm
chart archive. MySQL 8.0+ and Redis 7+ are runtime dependencies; Kafka is
required when `usage_events.driver=kafka`.

## Release archive

Download the archive for your platform and `SHA256SUMS` from the
[GitHub release](https://github.com/Zereker/llm-gateway/releases). Verify it
before extracting:

```sh
# Linux
sha256sum --check --ignore-missing SHA256SUMS

# macOS (replace the filename with the archive you downloaded)
grep 'llm-gateway_v0.1.0_darwin_arm64.tar.gz' SHA256SUMS | shasum -a 256 -c -
```

Each archive contains both commands, production configuration templates,
installation instructions, and the license. Confirm the embedded build metadata:

```sh
./llm-gateway -version
./llm-gateway-console -version
```

Install the binaries somewhere on `PATH`, copy the templates from `configs/`
to `/etc/llm-gateway/`, and inject secrets through the documented environment
variables. At minimum the gateway requires `LLM_GATEWAY_DATA_KEY`; the supplied
production gateway template contains the database connection. The Console also
requires `LLM_GATEWAY_DATABASE_DSN`, the same `LLM_GATEWAY_DATA_KEY`, and
`LLM_GATEWAY_CONSOLE_TOKENS`.

```sh
install -m 0755 llm-gateway llm-gateway-console /usr/local/bin/
install -d -m 0750 /etc/llm-gateway
install -m 0640 configs/gateway.yaml configs/console.yaml /etc/llm-gateway/

export LLM_GATEWAY_DATA_KEY="$(openssl rand -hex 32)"
llm-gateway -config /etc/llm-gateway/gateway.yaml
```

Do not generate separate data keys for Gateway and Console. Changing the key
without re-encrypting stored endpoint credentials makes those credentials
unreadable.

## Container images

Tagged releases publish separate images:

```sh
docker pull ghcr.io/zereker/llm-gateway:v0.1.0
docker pull ghcr.io/zereker/llm-gateway-console:v0.1.0
```

Both images run as UID/GID `65532:65532`. Mount the corresponding configuration
file read-only and supply secrets through your orchestrator. No `latest` tag is
published; use an immutable release tag or digest.

## Helm

The GitHub release includes `llm-gateway-0.1.0.tgz`. The chart deploys only the
Gateway data plane; MySQL, Redis, Kafka, and the optional Console remain external.

```sh
helm install ai-gw ./llm-gateway-0.1.0.tgz \
  --set-string secrets.databaseDSN='user:pwd@tcp(mysql:3306)/llm_gateway?parseTime=true&charset=utf8mb4' \
  --set-string secrets.dataKey="$(openssl rand -hex 32)"
```

For production, use `secrets.existingSecret` instead of command-line secret
values. See the chart's [deployment guide](../deploy/helm/llm-gateway/README.md).

## Build from source

Building requires the Go version declared by `go.mod`:

```sh
git clone https://github.com/Zereker/llm-gateway.git
cd llm-gateway
git checkout v0.1.0
make build VERSION=v0.1.0
./bin/llm-gateway -version
```

In regions where the default Go module proxy or container registry is slow,
all build entry points accept mirrors instead of requiring one fixed upstream:

```sh
GOPROXY=https://goproxy.cn,direct make build VERSION=v0.1.0

BASE_IMAGE_REGISTRY=docker.1ms.run \
GOPROXY=https://goproxy.cn,direct \
make docker-build VERSION=v0.1.0
```

The Quickstart additionally accepts `CONTAINER_REGISTRY`,
`BASE_IMAGE_REGISTRY`, and `GOPROXY` environment variables.

## Upgrade boundary

Read the [changelog](CHANGELOG.md) before upgrading. Starting with `v0.1.0`,
merged database migration files are immutable; upgrades add new numbered
migrations. Back up MySQL and preserve the endpoint credential data key before
changing a production deployment.
