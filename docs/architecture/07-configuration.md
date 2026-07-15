[English](07-configuration.md) | [简体中文](07-configuration.zh-CN.md)

# 07. Configuration

This file defines the target configuration contract for the gateway. Code implementation, example configs, and deployment templates should all treat this as the source of truth.

Configuration only describes process startup and infrastructure dependencies; business data such as
accounts, API keys, model services, subscriptions, endpoints, and quota policies are written directly
via SQL by the deployer, **not stored in `gateway.yaml`**.

## 1. Process configuration boundary

The repo has two binaries, each with its own config: `cmd/gateway` (the data plane), with config file
`gateway.yaml`, responsible for the HTTP server, per-request defaults, DB/Redis/Kafka/OTel connections,
scheduling, and plugin drivers; and `cmd/console` (the control plane), a separate Admin API for managing
business data. This document describes `gateway.yaml`; the console carries its own config.

The gateway requires a SQL DB and Redis to start. Kafka is only required when the usage-event driver is
set to `kafka`. Gateway startup applies pending versioned migrations
before validating the resulting schema.

The repo layer uses an in-process TTL LRU cache (`internal/repo/cache.go` + `internal/repo/cached.go`). Most
records rely on TTL; API-key revocation optionally uses best-effort cachebus invalidation. See
[06 §8](./06-pluggable-infra.md#8-repo-cache-deployer-sql--gateway-data-propagation) for details.

## 2. gateway.yaml

Full structure:

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
  # Synchronous Kafka is the acknowledged-delivery production default. Async
  # mode is an in-memory best-effort queue, not a transactional outbox.
  driver: kafka # file | kafka
  file:
    path: /var/log/llm-gateway/usage.jsonl   # required when driver=file
  kafka:
    brokers: ["kafka:9092"]
    # topic is named "domain.entity.event.version", decoupled from the producer service name (see docs/05 §5)
    topic: billing.usage.recorded.v1
    async: false               # true enables an in-memory best-effort queue
    # dlq_topic is used only in async mode and normally shares the broker failure domain
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

Field descriptions:

| Field | Required | Description |
|------|------|------|
| `server.addr` | Yes | gateway HTTP listen address |
| `request.body_limit_bytes` | Yes | per-request body size limit; oversized bodies are rejected at the router/server layer before M1 |
| `request.timeout` | Yes | total per-request processing timeout; implemented via gin TimeoutMiddleware wrapping the entire M1-M10 chain as a global fallback (used as the default when an upstream doesn't override its own timeout) |
| `database.driver` / `dsn` | Yes | SQL DB connection; target driver is MySQL |
| `redis.addr` | Yes | Redis connection; depended on by M6 rate limiting and scheduler cooldown |
| `data_key` | Yes | KEK used to decrypt endpoint auth ciphertext; the deployer must use the same KEK when encrypting for SQL INSERT |
| `usage_events.driver` | Yes | usage event output backend (`file` / `kafka`) |
| `scheduler.filters` | Yes | endpoint selection chain; `weighted_random` must run last |
| `selector.picker` | No | final pick strategy: `weighted_random` (default) / `p2c` (power-of-two-choices by pending calls) |
| `scheduler.max_attempts` | Yes | max endpoint attempts for the same model within a single request; can be lowered via header |
| `scheduler.cooldown.*` | Yes | mapping from `ErrorClass` to cooldown TTL; an upstream `Retry-After` / rate-limit reset hint overrides the static TTL, clamped to `[1s, 10m]` |
| `health.*` | No | active probing of self-hosted endpoints (default off); `health.recover_cooldown` enables probe-gated early cooldown release |
| `selector.session_affinity.*` | No | sticky routing via `X-Gateway-Session` (default off); `ttl` is the session→endpoint mapping lifetime |
| `cache.*` | No | response cache for chat + embedding modalities (default off); `cache.semantic.*` switches chat to similarity-based caching |
| `content_log.*` | No | request/response content logging channel; can be disabled |
| `trace.*` | Yes | slog / OTel driver and trace base fields |

## 3. Schema migration

Gateway startup applies pending versions and records them in `schema_migrations`.
Migration operations are idempotent and safe when replicas start concurrently. The gateway database
user therefore requires DDL permissions. Migration plus schema validation is bounded by a 30-second
startup deadline, so metadata-lock contention fails startup instead of hanging indefinitely.
Destructive changes still use an expand/migrate/contract
rollout and must complete before incompatible application code -- see
[00 §3 process startup order](./00-overview.md#3-running-processes).

## 4. Environment variable overrides

The target implementation should support overriding sensitive fields and deployment-specific fields via
environment variables. Recommended naming:

| Config field | Environment variable |
|----------|----------|
| `database.dsn` | `LLM_GATEWAY_DATABASE_DSN` |
| `redis.addr` | `LLM_GATEWAY_REDIS_ADDR` |
| `redis.password` | `LLM_GATEWAY_REDIS_PASSWORD` |
| `data_key` | `LLM_GATEWAY_DATA_KEY` |
| `usage_events.kafka.brokers` | `LLM_GATEWAY_KAFKA_BROKERS` |
| `moderation.api_key` | `LLM_GATEWAY_MODERATION_API_KEY` |
| `trace.endpoint` | `LLM_GATEWAY_OTEL_ENDPOINT` |

Environment variable overrides are applied after reading the YAML, and before default-value filling
and validation.

## 5. Validation rules

Fail-fast is split into two layers, each covering a different class of error:

**Validate() (inside Load, after ApplyDefaults)** -- validates constraints "that defaults can't fill in and must be supplied correctly by a human":

- `data_key` must be hex-encoded 32 bytes; the deployer must use the same KEK when encrypting endpoints.auth -- if inconsistent, the gateway fails to decrypt and all endpoints become unavailable. In production, inject this uniformly via a secret manager.
- `trace.driver` only accepts `slog|otel`; when `otel`, `endpoint` is required (the OTLP gRPC collector address).
- When `usage_events.driver=kafka`, `brokers` and `topic` are required; `async` selects acknowledged delivery or the in-memory best-effort queue.
- `content_log.driver` only accepts `none|file`; other values (including the legacy `kafka`) fail fast at startup.
- When `content_log.driver=file`, `file.path` is required.
- When `content_log.backpressure=block`, `block_timeout > 0` must be configured, to avoid blocking the response path indefinitely.
- `rate_limit.driver` only accepts `redis|inmemory`.
- `vendors.openai_compatible` entries must be non-empty, whitespace-free, and unique; collisions with a built-in **non-OpenAI** vendor (anthropic / gemini / cohere / bedrock / azureopenai) are caught at assembly time (`builtin.NewLookup` panics).

**Real connections at startup (inside buildEngine)** -- validates errors "that string checks can't catch":

- `database.dsn` / `redis.addr` have local-dev defaults (filled in by ApplyDefaults, so never empty);
  misconfiguration is exposed via the actual connection + ping fail-fast in `OpenDB` / `OpenRedis`.
- A typo in `scheduler.filters` names causes a panic in `buildSchedulerFilters` (unknown filter name
  fails fast); missing cooldown entries per class are filled in by ApplyDefaults.
- Endpoint business-data misconfiguration (protocol typo / unregistered vendor / metadata URL / quirks
  compile failure) is surfaced by the startup endpoint scan as a warning +
  `llm_gateway_endpoint_misconfigured_total` (does not block startup; see
  [00 §3](./00-overview.md#3-running-processes) step 6).

## 6. Evolution rules

Adding a new config field requires synchronized changes to:

- The struct, defaults, and validation in `internal/config`.
- `examples/local/configs`, `deploy/configs`, K8s values / configmap.
- This document.
- Any architecture chapters covering the related behavior, e.g. scheduler, rate limit, metering.

Deleting or renaming config fields does not need to preserve backward compatibility; the project is
still in its design phase.
