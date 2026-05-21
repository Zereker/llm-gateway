# 07. Configuration

本文件定义 gateway 的目标配置契约。代码实现、示例配置和部署模板都应以这里为准。

配置只描述进程启动和基础设施依赖；账号、API key、model service、subscription、
endpoint、quota policy 等业务数据由 deployer 直接写 SQL，**不放在 `gateway.yaml`**。

## 1. 进程配置边界

仓库只有一个 binary：`cmd/gateway`，配置文件 `gateway.yaml`，负责 HTTP server、
per-request 默认值、DB/Redis/Kafka/OTel 连接、调度和插件 driver。

gateway 启动必需 SQL DB 和 Redis。Kafka 只有在 outbox driver 选择 `kafka` /
`async_kafka` / `file_and_kafka` 时必需。gateway 启动期跑 `infra.Migrate` 建表
（`schema.sql` 全 `IF NOT EXISTS` 幂等）+ `repo.CheckSchema` 防御性校验。

repo 层用进程内 TTL LRU 缓存（`pkg/repo/cache.go` + `pkg/repo/cached.go`）；不需要
任何外部失效通道（CDC / Redis pub-sub 等）。详见
[06 §8](./06-pluggable-infra.md#8-repo-缓存deployer-sql--gateway-数据传播)。

## 2. gateway.yaml

完整结构：

```yaml
server:
  addr: ":8080"
  read_header_timeout: 10s
  shutdown_timeout: 30s

request:
  # per-request HTTP 默认限制（历史上叫 middleware:，但实际跟 M1-M10 链无关——
  # body_limit_bytes 在 M1 之前的 router/server 层拒掉；timeout 是包整条 M1-M10 链的全局兜底）
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

usage_events:
  # usage 事件下游通道（实现走 Outbox Pattern，pkg/usage.OutboxPublisher）。
  # 段名按"用途"暴露，跟 content_log: / trace: 风格一致；内部实现叫 Outbox 是模式名，
  # 操作面不可见。
  #
  # 生产推荐 file_and_kafka：file 是 source of truth（sync commit），Kafka 是异步
  # 广播；broker 挂不丢数据，由外部 replay 工具读 file 补发（详见 docs/05 §5）。
  driver: file_and_kafka # file | kafka | async_kafka | file_and_kafka
  file:
    path: /var/log/llm-gateway/usage.jsonl   # driver=file 或 file_and_kafka 时必填
  kafka:
    brokers: ["kafka:9092"]
    # topic 按"领域.实体.事件.版本"命名，跟生产者服务名解耦（见 docs/05 §5）
    topic: billing.usage.recorded.v1
    async: true
    # dlq_topic：单消息级错误（msg too large / schema invalid）兜底；
    # 在 file_and_kafka 下可选——file 已是 source of truth
    dlq_topic: billing.usage.recorded.v1.dlq
    client_id: llm-gateway
    acks: all
    compression: zstd
    buffer_size: 4096
    backpressure: drop_oldest # drop_oldest | block
    max_retries: 5
    retry_backoff: 500ms
    publish_timeout: 5s

selector:
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

# repo 层 TTL LRU 缓存的默认参数都写死在代码里（见 pkg/repo/cached.go），
# 不暴露到 yaml；需要调时直接改代码常量。

budget:
  driver: alwayspass # alwayspass | inmemory | external
  default_balance: 0

moderation:
  driver: none # none | openai | external
  api_key: ""
  base_url: ""
  timeout: 5s

content_log:
  # Content Log 只支持 none / file。gateway 故意不内嵌 Kafka producer——
  # 日志/审计性质的通道下游往往是多 sink（S3 + Loki + Kafka + ES），由
  # fluent-bit / vector 这一层负责扇出 + 重试，gateway 主进程只关心"写不影响响应"。
  # 详见 docs/05 §2。
  driver: none # none | file
  sample_rate: 1.0
  backpressure: drop_oldest # drop_oldest | drop_newest | block
  buffer_size: 1024
  file:
    # JSONL 追加写；文件轮转 / 压缩 / 清理由外部 logrotate 或日志收集器负责。
    path: /var/log/llm-gateway/content.jsonl

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
| `request.body_limit_bytes` | 是 | per-request body 上限；M1 之前的 router/server 层就拒掉超大 body |
| `request.timeout` | 是 | per-request 处理总超时；用 gin TimeoutMiddleware 包整条 M1-M10 链的全局兜底（不覆盖上游单独 timeout 时作为默认值） |
| `database.driver` / `dsn` | 是 | SQL DB 连接；目标 driver 是 MySQL |
| `redis.addr` | 是 | Redis 连接；M6 rate limit 和 scheduler cooldown 依赖 |
| `data_key` | 是 | endpoint auth 密文解密用 KEK；deployer SQL INSERT 加密时必须用同一个 KEK |
| `usage_events.driver` | 是 | usage event 输出后端（`file` / `kafka` / `async_kafka` / `file_and_kafka`，生产推荐 `file_and_kafka`） |
| `scheduler.filters` | 是 | endpoint 选择链；`weighted_random` 必须最后执行 |
| `scheduler.max_attempts` | 是 | 单次请求同 model 最大 endpoint 尝试次数，可被 header 降低 |
| `scheduler.cooldown.*` | 是 | `ErrorClass` 到 cooldown TTL 的映射 |
| `ratelimit.policy_cache_ttl` | 是 | quota policy 本地缓存 TTL |
| `content_log.*` | 否 | request/response 内容记录通道；可关闭 |
| `trace.*` | 是 | slog / OTel driver 和 trace 基础字段 |

## 3. Schema migration

gateway 启动期执行 `infra.Migrate` 把 `pkg/infra/schema.sql` 应用到数据库。
`schema.sql` 全用 `CREATE TABLE IF NOT EXISTS`，幂等可重复跑。

生产多副本部署：多个 gateway 实例同时启动时会各自跑 `infra.Migrate`，由于全部
DDL 都是 `IF NOT EXISTS`，并发跑只有"已存在"的 no-op，不会冲突。如果上线带破坏性
schema 变更（删字段 / 改类型），应通过外部部署系统保证迁移先在低流量窗口完成，
再滚动 gateway——参考 [00 §3 部署顺序](./00-overview.md#3-运行进程)。

## 4. 环境变量覆盖

目标实现应支持用环境变量覆盖敏感字段和部署差异字段。推荐命名：

| 配置字段 | 环境变量 |
|----------|----------|
| `database.dsn` | `LLM_GATEWAY_DATABASE_DSN` |
| `redis.addr` | `LLM_GATEWAY_REDIS_ADDR` |
| `redis.password` | `LLM_GATEWAY_REDIS_PASSWORD` |
| `data_key` | `LLM_GATEWAY_DATA_KEY` |
| `usage_events.kafka.brokers` | `LLM_GATEWAY_KAFKA_BROKERS` |
| `moderation.api_key` | `LLM_GATEWAY_MODERATION_API_KEY` |
| `trace.endpoint` | `LLM_GATEWAY_OTEL_ENDPOINT` |

环境变量覆盖发生在读取 YAML 之后、执行默认值填充和校验之前。

## 5. 校验规则

配置加载必须 fail fast：

- `database.driver` / `database.dsn` 为空时拒绝启动。
- `redis.addr` 为空或 ping 失败时拒绝启动。
- `data_key` 必须是 hex encoded 32 bytes；deployer 写 endpoints.auth 时必须用同一个 KEK 加密——不一致时 gateway 解密失败，所有 endpoint 不可用。生产用 secret manager 统一注入。
- `usage_events.driver=kafka|async_kafka` 时，`brokers` 和 `topic` 必填。
- `usage_events.driver=file_and_kafka` 时，**同时**要求 `file.path` 非空（source of truth）+ `kafka.brokers` 非空 + `kafka.topic` 非空。
- `usage_events.kafka.compression=zstd` 需要 broker ≥ 2.1；旧 broker 应改为 `lz4` 或 `snappy`，否则 producer 创建失败，启动 fail-fast。
- `trace.driver=otel` 时 `endpoint` 必填；driver=slog 时可忽略。
- `usage_events.driver=async_kafka` 且 `backpressure=block` 时必须配置 `publish_timeout`，避免无限阻塞响应收尾。
- `scheduler.filters` 必须包含一个最终 selector，目标第一版是 `weighted_random`。
- `scheduler.cooldown` 必须覆盖全部 `ErrorClass`。
- `content_log.driver` 只接受 `none|file`；其他值（包括历史的 `kafka`）启动期 fail-fast。
- `content_log.driver=file` 时 `file.path` 必填，否则启动期 fail-fast。
- `content_log.backpressure=block` 时必须配置发布 timeout，避免无限阻塞响应流。

## 6. 演进规则

新增配置字段时必须同步修改：

- `pkg/config` 的结构、默认值和校验。
- `configs/local`、`configs/prod`、K8s values / configmap。
- 本文档。
- 涉及行为的 architecture 章节，例如 scheduler、rate limit、metering。

删除或改名配置字段不需要兼容旧版本；项目仍处于设计阶段。
