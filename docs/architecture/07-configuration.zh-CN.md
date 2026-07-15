[English](07-configuration.md) | [简体中文](07-configuration.zh-CN.md)

# 07. 配置

该文件定义网关的目标配置合约。代码实现、示例配置和部署模板都应将此视为事实来源。

配置仅描述进程启动和基础设施依赖关系；业务数据，例如
账户、API密钥、模型服务、订阅、端点和配额策略直接写入
部署者通过 SQL，**不存储在 `gateway.yaml`**。

## 1.进程配置边界

该仓库有两个二进制文件，每个都有自己的配置：`cmd/gateway`（数据平面），带有配置文件
`gateway.yaml`，负责HTTP服务器，每个请求默认值，DB/Redis/Kafka/OTel连接，
调度和插件驱动程序；和 `cmd/console`（控制平面），用于管理的单独管理 API
业务数据。本文档描述了`gateway.yaml`；控制台有自己的配置。

网关需要 SQL DB 和 Redis 才能启动。仅当用量事件驱动设置为 `kafka` 时才需要 Kafka。
网关启动应用挂起的版本化迁移
在验证结果模式之前。

存储库层使用进程内 TTL LRU 缓存 (`internal/repo/cache.go` + `internal/repo/cached.go`)。大多数
记录依赖于TTL； API 密钥撤销可以选择使用尽力而为的缓存总线失效。参见
[06 §8](./06-pluggable-infra.zh-CN.md#8-repo-cache-deployer-sql--gateway-data-propagation) 了解详细信息。

<a id="2-gatewayyaml"></a>
## 2. gateway.yaml

完整结构：

```yaml
server:
  addr: ":8080"
  read_header_timeout: 10s
  shutdown_timeout: 30s

request:
  # per-request HTTP default limits (historically called middleware:, but actually unrelated to the
  # M1-M10 chain -- body_limit_bytes is rejected at the router/server layer before M1; timeout wraps
  # the entire M1-M10 chain as a global fallback)
  body_limit_bytes: 10485760
  timeout: 60s

database:
  driver: mysql
  dsn: "user:pass@tcp(mysql:3306)/llm_gateway?parseTime=true&charset=utf8mb4"
  max_open_conns: 50

rate_limit:
  # M6 counter store: redis (default; fleet-wide counters, required for
  # multi-replica) | inmemory (process-local counters, identical sliding-window
  # semantics; single-replica / local dev / tests only -- see docs/04 §5)
  driver: redis

vendors:
  # Deployment-local OpenAI-compatible vendor names, registered on the shared
  # OpenAI Factory at startup -- extends the compiled-in list in
  # internal/protocol/openai.Aliases() without a rebuild. Endpoint SQL rows can
  # then use these names as `vendor` with `protocol: openai`. Names colliding
  # with a built-in non-OpenAI vendor fail startup.
  openai_compatible: []   # e.g. ["acme-llm"]
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
  # Downstream channel for usage events (implemented via internal/usage.OutboxPublisher).
  # The section is named by "purpose", consistent with the content_log: / trace: style; the internal
  # implementation interface name is not exposed on the operational surface.
  # 同步 Kafka 是生产环境默认的确认投递方式。异步模式是内存 best-effort 队列，
  # 不是事务 Outbox。
  driver: kafka # file | kafka
  file:
    path: /var/log/llm-gateway/usage.jsonl   # required when driver=file
  kafka:
    brokers: ["kafka:9092"]
    # topic is named "domain.entity.event.version", decoupled from the producer service name (see docs/05 §5)
    topic: billing.usage.recorded.v1
    async: false               # true enables an in-memory best-effort queue
    # dlq_topic 只用于异步模式，通常与主 Topic 共享 Broker 故障域
    dlq_topic: billing.usage.recorded.v1.dlq
    buffer_size: 4096         # async channel capacity; 0 = default 1024
    max_retries: 5            # max retries per async event; 0 = default 3
    backoff_base: 200ms       # exponential backoff starting point; 0 = default 200ms
    # Note: legacy fields such as client_id / acks / compression / backpressure / publish_timeout
    # no longer exist (the config struct has no corresponding fields; setting them is silently
    # ignored); producer-level parameters are fixed at code defaults where internal/infra KafkaWriter
    # is constructed

selector:
  filters: [cooldown, limit_read, weighted_random]
  # picker: final pick strategy after filters + scoring.
  #   weighted_random (default) = pure EffectiveWeight-weighted random
  #   p2c = power-of-two-choices: sample two candidates by weight, take the
  #         one with fewer pending calls (live-load aware; see docs/03 §4)
  picker: weighted_random # weighted_random | p2c
  max_attempts: 3
  cooldown:
    # Static per-class TTLs. When a failed upstream response carries its own
    # recovery hint (Retry-After / rate-limit reset headers), the hint
    # overrides these, clamped to [1s, 10m] (docs/03 §9). A class set to 0s
    # never cools down, hint or not.
    transient: 30s
    capacity: 60s
    permanent: 5m
    invalid: 0s
    unknown: 10s
  session_affinity:
    # Sticky routing: the client's X-Gateway-Session header pins a session to
    # the same upstream endpoint (for prefix / KV cache hits). Redis-backed
    # (shared across replicas); no effect when enabled=false.
    enabled: false
    ttl: 10m # TTL for the session -> endpoint mapping; default 10m

health:
  # Active probing of self-hosted endpoints (docs/03 §10); default off.
  enabled: false
  interval: 30s
  timeout: 5s
  concurrent: 8
  # recover_cooldown: a successful probe of an endpoint in cooldown clears
  # the cooldown early (probe-gated recovery; release-only, a failed probe
  # never creates or extends a cooldown).
  recover_cooldown: false

scoring:
  # Runtime Scoring (opt-in, disabled by default): soft-weights endpoints based on runtime quality.
  # When enabled=true, Scheduler.Report writes EMA statistics, and at Pick time DefaultScorer adjusts
  # EffectiveWeight based on the success/latency factor (the hard filter decides whether an endpoint
  # is eligible; scoring decides which eligible endpoint is preferred). See 03-endpoint-scheduling §8.
  enabled: false
  # driver: stats storage backend. inmemory = each replica accumulates independently (single replica /
  # tolerates divergence); redis = all replicas share the same per-endpoint EMA, giving consistent
  # scoring (for production multi-replica deployments).
  driver: inmemory        # inmemory | redis
  min_samples: 5          # below this sample count, use a neutral factor=1 to preserve exploration traffic for new endpoints
  latency_baseline: 200ms # latency factor = baseline / actual
  ema_decay: 0.2          # EMA decay (weight of new samples)
  stats_ttl: 1h           # per-endpoint stats TTL under the redis driver

cache:
  # Response cache (chat + embedding modalities): a hit returns directly,
  # skipping the upstream. Redis-backed (shared across replicas). Runs between
  # M6 Limit and M7 Schedule; a no-op when enabled=false. By default only
  # non-streaming + temperature=0 deterministic requests are cached; the
  # client's X-Gateway-Cache header can override this.
  enabled: false
  ttl: 5m # default 5m
  semantic:
    # When enabled=true, use the semantic cache instead of exact match: embed
    # the prompt and hit by cosine similarity (paraphrases hit too). Replaces
    # the exact cache for chat; embedding routes always use exact match.
    enabled: false
    threshold: 0.9    # cosine hit threshold (default 0.9)
    max_entries: 500  # entry cap per namespace (default 500)
    embedder:
      driver: openai  # OpenAI-compatible /v1/embeddings
      api_key: ""
      base_url: ""
      model: text-embedding-3-small # default text-embedding-3-small

# Note: the ratelimit quota-policy cache TTL is not a yaml field -- main.go
# constructs ratelimit.NewPolicyCache(reader, 0). The repo layer's TTL LRU cache
# parameters are likewise hardcoded (see internal/repo/cached.go) and not exposed in
# yaml; change the code constants directly if tuning is needed.

budget:
  driver: alwayspass # alwayspass | inmemory
  default_balance: 0

moderation:
  driver: none # none | openai
  api_key: ""
  base_url: ""

content_log:
  # Content Log only supports none / file. The gateway deliberately does not embed a Kafka producer --
  # logging/audit-style channels typically fan out to multiple sinks downstream (S3 + Loki + Kafka + ES),
  # and that fan-out + retry responsibility belongs to a layer like fluent-bit / vector; the gateway
  # main process only cares that "writing doesn't affect the response".
  # See docs/05 §2 for details.
  driver: none # none | file
  sample_rate: 1.0
  backpressure: drop_oldest # drop_oldest | drop_newest | block
  buffer_size: 1024
  file:
    # JSONL append-only writes; file rotation / compression / cleanup is handled externally by
    # logrotate or the log collector.
    path: /var/log/llm-gateway/content.jsonl

trace:
  driver: slog # slog | otel
  service_name: llm-gateway
  endpoint: "" # required when driver=otel (OTLP gRPC collector address)
```

字段说明：

|领域|必填 |描述 |
|------|------|------|
| `server.addr` |是的 |网关HTTP监听地址|
| `request.body_limit_bytes` |是的 |每个请求的正文大小限制；过大的实体在 M1 之前在路由器/服务器层被拒绝 |
| `request.timeout` |是的 |每个请求处理总超时；通过 gin TimeoutMiddleware 实现，将整个 M1-M10 链包装为全局回退（当上游不覆盖其自己的超时时用作默认值） |
| `database.driver` / `dsn` |是的 | SQL数据库连接；目标驱动程序是MySQL |
| `redis.addr` |是的 | Redis连接；取决于M6速率限制和调度程序冷却时间|
| `data_key` |是的 | KEK用于解密端点认证密文；部署者在加密 SQL INSERT 时必须使用相同的 KEK |
| `usage_events.driver` |是的 |使用事件输出后端（`file` / `kafka`）|
| `scheduler.filters` |是的 |端点选择链； `weighted_random` 必须最后运行 |
| `selector.picker` |没有 |最终选择策略：`weighted_random`（默认）/ `p2c`（待处理呼叫的两种选择）|
| `scheduler.max_attempts` |是的 |单个请求中同一模型的最大端点尝试次数；可以通过 header | 降低
| `scheduler.cooldown.*` |是的 |从 `ErrorClass` 映射到冷却 TTL；上游 `Retry-After` / 速率限制重置提示会覆盖静态 TTL，限制为 `[1s, 10m]` |
| `health.*` |没有 |主动探测自托管端点（默认关闭）； `health.recover_cooldown` 启用探针门控早期冷却释放 |
| `selector.session_affinity.*` |没有 |通过 `X-Gateway-Session` 的粘性路由（默认关闭）； `ttl` 是会话→端点映射生命周期 |
| `cache.*` |没有 |聊天+嵌入模式的响应缓存（默认关闭）； `cache.semantic.*` 将聊天切换到基于相似性的缓存 |
| `content_log.*` |没有 |请求/响应内容记录通道；可以禁用 |
| `trace.*` |是的 | slog / OTel 驱动程序和跟踪基本字段 |

## 3.架构迁移

网关启动应用挂起版本并将其记录在 `schema_migrations` 中。
当副本同时启动时，迁移操作是幂等且安全的。网关数据库
因此用户需要 DDL 权限。迁移加架构验证的时间限制为 30 秒
启动截止时间，因此元数据锁争用会导致启动失败，而不是无限期挂起。
破坏性更改仍然使用扩展/迁移/收缩
推出并且必须在不兼容的应用程序代码之前完成 - 请参阅
[00 §3进程启动顺序](./00-overview.zh-CN.md#3-running-processes)。

## 4.环境变量覆盖

目标实现应支持通过覆盖敏感字段和特定于部署的字段
环境变量。推荐命名：

|配置字段|环境变量|
|----------|----------|
| `database.dsn` | `LLM_GATEWAY_DATABASE_DSN` |
| `redis.addr` | `LLM_GATEWAY_REDIS_ADDR` |
| `redis.password` | `LLM_GATEWAY_REDIS_PASSWORD` |
| `data_key` | `LLM_GATEWAY_DATA_KEY` |
| `usage_events.kafka.brokers` | `LLM_GATEWAY_KAFKA_BROKERS` |
| `moderation.api_key` | `LLM_GATEWAY_MODERATION_API_KEY` |
| `trace.endpoint` | `LLM_GATEWAY_OTEL_ENDPOINT` |

环境变量覆盖在读取 YAML 之后、默认值填充之前应用
和验证。

## 5. 验证规则

快速失败分为两层，每层涵盖不同类别的错误：

**Validate()（在 Load 内部，ApplyDefaults 之后）** -- 验证“默认值无法填写且必须由人类正确提供”的约束：

- `data_key` 必须是十六进制编码的32字节；部署者在加密端点时必须使用相同的 KEK。auth - 如果不一致，网关将无法解密并且所有端点将变得不可用。在生产中，通过秘密管理器统一注入。
- `trace.driver`仅接受`slog|otel`；当需要 `otel` 时，需要 `endpoint`（OTLP gRPC 收集器地址）。
- 当 `usage_events.driver=kafka` 时，`brokers` 和 `topic` 必填；`async` 用于选择确认投递或内存 best-effort 队列。
- `content_log.driver`仅接受`none|file`；其他值（包括旧版 `kafka`）在启动时会快速失败。
- 当需要 `content_log.driver=file` 时，`file.path` 是必需的。
- 当必须配置`content_log.backpressure=block`时，必须配置`block_timeout > 0`，以避免无限期阻塞响应路径。
- `rate_limit.driver` 仅接受 `redis|inmemory`。
- `vendors.openai_compatible` 条目必须非空、无空格且唯一；与内置**非 OpenAI** 供应商（anthropic / gemini / cohere / bedrock / azureopenai）的冲突在组装时被捕获（`builtin.NewLookup` 恐慌）。

**启动时的真实连接（在 buildEngine 内）** -- 验证“字符串检查无法捕获”的错误：

- `database.dsn` / `redis.addr` 具有本地开发默认值（由ApplyDefaults填充，因此永远不会为空）；
  错误配置通过 `OpenDB` / `OpenRedis` 中的实际连接 + ping 快速失败暴露出来。
- `scheduler.filters` 名称中的拼写错误会导致 `buildSchedulerFilters` 出现恐慌（未知过滤器名称
  快速失败）；每个类别缺少的冷却条目由ApplyDefaults 填充。
- 端点业务数据配置错误（协议拼写错误/未注册供应商/元数据 URL/Quirks
  编译失败）由启动端点扫描作为警告显示+
  `llm_gateway_endpoint_misconfigured_total`（不阻止启动；请参阅
  [00§3](./00-overview.zh-CN.md#3-running-processes)步骤6)。

## 6.演进规则

添加新的配置字段需要同步更改：

- `internal/config` 中的结构、默认值和验证。
- `examples/local/configs`、`deploy/configs`、K8s 值/configmap。
- 本文件。
- 涵盖相关行为的任何架构章节，例如调度程序、速率限制、计量。

删除或重命名配置字段不需要保留向后兼容性；该项目是
仍处于设计阶段。
