# configs/

每个子目录是一份**完整的、自包含的**网关配置，对应一个环境。

## 结构

```
configs/
├── local/                  # 本地开发配置；依赖 docker compose 的 MySQL + Redis + Redpanda
│   ├── gateway.yaml        # server / middleware / database / redis / outbox
│   └── admin.yaml          # admin server / database / data_key
│
└── prod/                   # 生产模板
    ├── gateway.yaml        # 外部 SQL DB / Redis / Kafka / secrets 占位
    └── admin.yaml
```

本项目目标架构不提供零外部依赖启动。gateway 启动必需 SQL DB 和 Redis：

- SQL DB 保存 accounts、api_keys、model_services、subscriptions、endpoints、quota_policies 等。
- Redis 承载 M6 rate limit buckets 和 scheduler cooldown。
- Kafka/Redpanda 仅在 outbox driver 选择 kafka 时必需；file outbox 可用于本地调试。

schema 由 admin 进程或部署流程维护；gateway 启动只做 `repo.CheckSchema`，不建表、不迁移。

## 路径解析

配置文件中的外部依赖地址应指向当前环境的 SQL DB、Redis 和 outbox 后端。生产环境不要把真实凭证写入 git；通过 Secret、环境变量渲染或部署系统注入。

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
  driver: file # file | kafka
  file:
    path: /tmp/llm-gateway-usage.log
  kafka:
    brokers: ["kafka:9092"]
    topic: billing.usage.recorded.v1
    async: true
    buffer_size: 1024
    max_retries: 3
    dlq_topic: billing.usage.recorded.v1.dlq

scheduler:
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
# 编辑 configs/staging/gateway.yaml + admin.yaml
# 启动 admin 维护 schema，并录入 account / api_key / model_service / endpoint
go run ./cmd/gateway -config ./configs/staging/gateway.yaml
```

## 密钥管理

**绝对不要把真实密钥 commit 到 git。** 推荐：

| 场景 | 方案 |
|------|------|
| local dev | 使用 docker compose 启动 MySQL / Redis / Redpanda；测试 endpoint/API key 由 admin 写入 DB |
| CI / staging | 独立 SQL DB / Redis；密钥由 CI secret 或部署系统注入 |
| prod | SQL DB / Redis / Kafka 使用托管服务；凭证走 Vault / secret manager / K8s Secret |

`prod/gateway.yaml` 中的 DSN、Redis 密码、data key、上游 API key 都必须由部署环境注入，不应提交真实值。
