# 07. Configuration

本文件定义 gateway / admin 的目标配置契约。代码实现、示例配置和部署模板都应以这里为准。

配置只描述进程启动和基础设施依赖；账号、API key、model service、subscription、endpoint、quota policy 等业务配置由 admin 写入 SQL DB，不放在 `gateway.yaml`。

## 1. 进程配置边界

| 进程 | 配置文件 | 负责内容 |
|------|----------|----------|
| gateway | `gateway.yaml` | HTTP server、middleware、DB/Redis/Kafka/OTel 连接、调度和插件 driver |
| admin | `admin.yaml` | admin HTTP server、DB 连接、schema migration、data key |

gateway 启动必需 SQL DB 和 Redis。Kafka 只有在 outbox driver 选择 `kafka` / `async_kafka` 时必需。gateway 启动只执行 `repo.CheckSchema`，不创建表、不迁移 schema。

## 2. gateway.yaml

完整结构：

```yaml
server:
  addr: ":8080"
  read_header_timeout: 10s
  shutdown_timeout: 30s

middleware:
  body_limit_bytes: 10485760
  timeout: 60s

database:
  driver: mysql
  dsn: "user:pass@tcp(mysql:3306)/llm_gateway?parseTime=true&charset=utf8mb4"
  max_open_conns: 50
  max_idle_conns: 10
  conn_max_lifetime: 30m

redis:
  addr: "redis:6379"
  db: 0
  password: ""
  dial_timeout: 5s
  read_timeout: 2s
  write_timeout: 2s

data_key: "<hex-encoded-32-byte-key>"

outbox:
  driver: file # file | kafka | async_kafka
  file:
    path: /var/log/llm-gateway/usage.jsonl
  kafka:
    brokers: ["kafka:9092"]
    topic: llm-gateway.usage
    dlq_topic: llm-gateway.usage.dlq
    client_id: llm-gateway
    acks: all
    compression: zstd
    buffer_size: 4096
    backpressure: drop_oldest # drop_oldest | block
    max_retries: 5
    retry_backoff: 500ms
    publish_timeout: 5s

scheduler:
  filters: [cooldown, limit_read, weighted_random]
  max_attempts: 3
  cooldown:
    transient: 30s
    capacity: 60s
    permanent: 5m
    invalid: 0s
    unknown: 10s
  # scoring: deferred — runtime scoring 未在 v0.3 实现，配置块占位仅作未来契约示意；
  # 落地节奏见 03-endpoint-scheduling §8 Runtime Scoring（后续演进）。
  # scoring:
  #   enabled: false
  #   window: 5m
  #   latency_weight: 0.25
  #   success_weight: 0.50
  #   cost_weight: 0.25

ratelimit:
  policy_cache_ttl: 30s
  redis_prefix: llm-gateway:ratelimit

budget:
  driver: alwayspass # alwayspass | inmemory | external
  default_balance: 0

moderation:
  driver: none # none | openai | external
  api_key: ""
  base_url: ""
  timeout: 5s

content_log:
  driver: none # none | file | kafka | external
  sample_rate: 1.0
  backpressure: drop_oldest # drop_oldest | drop_newest | block
  buffer_size: 1024
  file:
    path: /var/log/llm-gateway/content.jsonl
  kafka:
    brokers: ["kafka:9092"]
    topic: llm-gateway.content # Content Log 默认 topic；与 Usage Event 的 llm-gateway.usage 分离

trace:
  driver: slog # slog | otel
  service_name: llm-gateway
  endpoint: ""
  sample_ratio: 1.0
  headers:
    request_id: X-Request-ID
```

字段说明：

| 字段 | 必需 | 说明 |
|------|------|------|
| `server.addr` | 是 | gateway HTTP listen 地址 |
| `middleware.body_limit_bytes` | 是 | 请求 body 上限，M1 执行 |
| `middleware.timeout` | 是 | 请求处理总超时，不覆盖上游单独 timeout 时作为默认值 |
| `database.driver` / `dsn` | 是 | SQL DB 连接；目标 driver 是 MySQL |
| `redis.addr` | 是 | Redis 连接；M6 rate limit 和 scheduler cooldown 依赖 |
| `data_key` | 是 | endpoint auth 密文解密用 KEK，必须与 admin 一致 |
| `outbox.driver` | 是 | usage event 输出后端 |
| `scheduler.filters` | 是 | endpoint 选择链；`weighted_random` 必须最后执行 |
| `scheduler.max_attempts` | 是 | 单次请求同 model 最大 endpoint 尝试次数，可被 header 降低 |
| `scheduler.cooldown.*` | 是 | `ErrorClass` 到 cooldown TTL 的映射 |
| `ratelimit.policy_cache_ttl` | 是 | quota policy 本地缓存 TTL |
| `content_log.*` | 否 | request/response 内容记录通道；可关闭 |
| `trace.*` | 是 | slog / OTel driver 和 trace 基础字段 |

## 3. admin.yaml

完整结构：

```yaml
server:
  addr: ":8081"
  read_header_timeout: 10s
  shutdown_timeout: 30s

database:
  driver: mysql
  dsn: "user:pass@tcp(mysql:3306)/llm_gateway?parseTime=true&charset=utf8mb4"
  max_open_conns: 20
  max_idle_conns: 5
  conn_max_lifetime: 30m

data_key: "<hex-encoded-32-byte-key>"

migration:
  enabled: true
  lock_key: llm-gateway:schema-migrate
  lock_ttl: 30s

trace:
  driver: slog
  service_name: llm-gateway-admin
  endpoint: ""
```

admin 启动期执行 `infra.Migrate`。生产多副本部署时必须通过 DB lock 或外部部署系统保证同一时间只有一个 migration owner。

## 4. 环境变量覆盖

目标实现应支持用环境变量覆盖敏感字段和部署差异字段。推荐命名：

| 配置字段 | 环境变量 |
|----------|----------|
| `database.dsn` | `LLM_GATEWAY_DATABASE_DSN` |
| `redis.addr` | `LLM_GATEWAY_REDIS_ADDR` |
| `redis.password` | `LLM_GATEWAY_REDIS_PASSWORD` |
| `data_key` | `LLM_GATEWAY_DATA_KEY` |
| `outbox.kafka.brokers` | `LLM_GATEWAY_KAFKA_BROKERS` |
| `moderation.api_key` | `LLM_GATEWAY_MODERATION_API_KEY` |
| `trace.endpoint` | `LLM_GATEWAY_OTEL_ENDPOINT` |

环境变量覆盖发生在读取 YAML 之后、执行默认值填充和校验之前。

## 5. 校验规则

配置加载必须 fail fast：

- `database.driver` / `database.dsn` 为空时拒绝启动。
- `redis.addr` 为空或 ping 失败时拒绝启动。
- `data_key` 必须是 hex encoded 32 bytes，且 **gateway 与 admin 必须配置完全一致**；不一致时 gateway 解密 endpoint auth 全部失败，所有 endpoint 不可用。建议通过同一 secret manager 注入，避免分别维护。
- `outbox.driver=kafka|async_kafka` 时，`brokers` 和 `topic` 必填。
- `outbox.kafka.compression=zstd` 需要 broker ≥ 2.1；旧 broker 应改为 `lz4` 或 `snappy`，否则 producer 创建失败，启动 fail-fast。
- `trace.driver=otel` 时 `endpoint` 必填；driver=slog 时可忽略。
- `outbox.driver=async_kafka` 且 `backpressure=block` 时必须配置 `publish_timeout`，避免无限阻塞响应收尾。
- `scheduler.filters` 必须包含一个最终 selector，目标第一版是 `weighted_random`。
- `scheduler.cooldown` 必须覆盖全部 `ErrorClass`。
- `content_log.backpressure=block` 时必须配置发布 timeout，避免无限阻塞响应流。

## 6. 演进规则

新增配置字段时必须同步修改：

- `pkg/config` 的结构、默认值和校验。
- `configs/local`、`configs/prod`、K8s values / configmap。
- 本文档。
- 涉及行为的 architecture 章节，例如 scheduler、rate limit、metering。

删除或改名配置字段不需要兼容旧版本；项目仍处于设计阶段。
