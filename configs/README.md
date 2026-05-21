# configs/

每个子目录是一份**完整的、自包含的**网关配置，对应一个环境。

## 结构

```
configs/
├── local/                  # 本地开发配置；依赖 docker compose 的 MySQL + Redis + Redpanda
│   └── gateway.yaml
│
├── prod/                   # 生产模板
│   └── gateway.yaml
│
├── docker/                 # docker-compose 用的容器版配置（仅供 image 内引用）
│   └── gateway.yaml
│
├── debezium/               # debezium server 配置（CDC：MySQL binlog → Redis Stream）
└── mysql-init/             # MySQL 容器初始化脚本（默认参数）
```

本项目目标架构**不提供零外部依赖启动**。gateway 启动必需 SQL DB 和 Redis：

- SQL DB 保存 accounts / api_keys / model_services / subscriptions / endpoints /
  quota_policies / pricing_versions 等业务表。
- Redis 承载 M6 rate limit buckets、scheduler cooldown 和 CDC stream 缓存。
- Kafka/Redpanda 仅在 outbox driver 选择 kafka 时必需；file outbox 可用于本地调试。

schema 由 gateway 启动期自跑 `pkg/infra.Migrate` 建表（`schema.sql` 全 `IF NOT EXISTS`
幂等）；`repo.CheckSchema` 在 Migrate 之后做防御性校验。

业务数据（accounts / endpoints / api_keys 等）由 deployer 直接 SQL 写入 ——
本项目只做数据面，不带控制面 REST API。

## 路径解析

配置文件中的外部依赖地址应指向当前环境的 SQL DB、Redis 和 outbox 后端。
生产环境不要把真实凭证写入 git；通过 Secret、环境变量渲染或部署系统注入。

## Gateway 配置结构

`gateway.yaml` 顶层结构：

```yaml
server:
  addr: ":8080"
  read_header_timeout: 10s
  shutdown_timeout: 30s

request:
  body_limit_bytes: 10485760
  timeout: 60s

database:
  driver: mysql
  dsn: "user:pass@tcp(mysql:3306)/llm_gateway?parseTime=true&charset=utf8mb4"

redis:
  addr: "redis:6379"
  db: 0
  password: ""

data_key: "<hex-encoded-32-byte-key>"

usage_events:
  driver: file # file | kafka | async_kafka | file_and_kafka
  file:
    path: /tmp/llm-gateway-usage.log
  kafka:
    brokers: ["kafka:9092"]
    topic: billing.usage.recorded.v1
    async: true
    buffer_size: 1024
    max_retries: 3
    dlq_topic: billing.usage.recorded.v1.dlq

selector:
  filters: [cooldown, limit_read, weighted_random]
  max_attempts: 3
  cooldown:
    transient: 30s
    capacity: 60s
    permanent: 5m
    invalid: 0s
    unknown: 10s

budget:
  driver: alwayspass # alwayspass | inmemory
  default_balance: 0

moderation:
  driver: none # none | openai
  api_key: ""
  base_url: ""

trace:
  driver: slog # slog | otel
  endpoint: ""
  service_name: llm-gateway
```

新增配置字段时，需要同步更新 `pkg/config`、示例配置、本文件和对应 architecture 文档。

## 添加新环境

```sh
cp -r configs/local configs/staging
# 编辑 configs/staging/gateway.yaml
go run ./cmd/gateway -config ./configs/staging/gateway.yaml
# 启动后用 SQL INSERT 录入业务数据（见 examples/full-config/README.md 的"数据管理"章节）
```

## 密钥管理

**绝对不要把真实密钥 commit 到 git。** 推荐：

| 场景 | 方案 |
|------|------|
| local dev | 使用 docker compose 启动 MySQL / Redis / Redpanda；测试 endpoint/API key 直接 SQL INSERT |
| CI / staging | 独立 SQL DB / Redis；密钥由 CI secret 或部署系统注入 |
| prod | SQL DB / Redis / Kafka 使用托管服务；凭证走 Vault / secret manager / K8s Secret |

`prod/gateway.yaml` 中的 DSN、Redis 密码、data key、上游 API key 都必须由部署环境注入，
不应提交真实值。
