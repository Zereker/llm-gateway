# 06 — Pluggable Infrastructure

This document records the pluggable boundaries of the infrastructure, and the dependency direction among domain / middleware / repo. This document is the target boundary — it does not describe legacy implementations for compatibility; code should converge toward this document.

Core principles:

1. `pkg/domain` defines the gateway's business model, does not reference `pkg/repo`, and must not use repo struct aliases.
2. `pkg/middleware` defines the minimal dependency interfaces it needs itself.
3. `pkg/repo` is the SQL implementation layer, and can implement the interfaces defined by middleware.
4. repo interfaces and implementations return `domain` structs, instead of leaking repo models to the upper layer.
5. middleware construction uses the option pattern, to make it easy to inject stub / fake in unit tests.

The existence of `pkg/domain` should not be just an import-path wrapper. If domain points to repo via a type alias, the business layer appears to depend on domain, but actually still drags SQL schema, ORM tags, and Scanner / Valuer into packages like schedule, translator, and upstream. Later, when replacing the storage implementation, writing unit tests, or adjusting the table structure, you'll get dragged back by the repo type.

## 1. Dependency Direction

Target dependency direction:

```text
pkg/domain                         pure business structs, no repo import
      ▲
      ├── pkg/middleware           defines middleware's minimal interfaces, calls schedule / upstream
      │       └── pkg/selector     pure scheduling logic and eligibility, holds no repo
      │
      └── pkg/repo                 SQL schema model + SQL implementation, adapts and returns domain types

cmd/gateway                         wires up repo, middleware, schedule and infra drivers
```

Forbidden directions:

```text
domain -> repo
middleware -> repo concrete model
repo interface -> middleware-only contract leakage
```

The purpose of doing this:

- domain is not polluted by SQL tags / gorm tags / Scanner / Valuer.
- middleware unit tests don't need to construct repo models or SQL structs.
- repo can freely adjust its storage structure, as long as it adapts to domain output.
- when replacing the SQL implementation, you only need to implement middleware's small interface.

## 2. Startup Dependencies

Target startup dependencies of `cmd/gateway`:

| Dependency | Purpose | Required |
|------|------|----------|
| YAML config | server, database, redis, middleware, scheduler, outbox, trace, etc. configuration | Required |
| SQL DB | main accounts, API keys, model service, subscription, endpoint, quota policy | Required |
| Redis | M6 rate limit, scheduler cooldown | Required |
| file or Kafka outbox | usage event output | Required, pick one |
| OTel collector | used when trace driver is `otel` | Optional |
| OpenAI moderation API | used when moderation driver is `openai` | Optional |

The DB schema source of truth is `pkg/infra/schema.sql`. gateway only runs `repo.CheckSchema` — it does not AutoMigrate and does not create tables.

Pricing does not do active price lookups on the gateway's hot path; price matching and amount calculation are done by the downstream billing platform based on when the request occurred.

## 3. Domain Model

`pkg/domain` should only contain the structs needed by the gateway's business layer, for example:

- `UserIdentity`
- `Credentials`
- `ModelService`
- `Endpoint`
- `QuotaPolicy` / `QuotaRule`
- `RequestEnvelope`
- `Usage`
- `SchedulingDecision`

Requirements for domain structs:

- No `db` / `gorm` tags.
- Do not implement SQL `Scanner` / `Valuer`.
- Do not import `pkg/repo`.
- Fields express business semantics, not table structure.

Counter-example:

```go
type Endpoint = repo.Endpoint
```

Target:

```go
package domain

type Endpoint struct {
    ID           int64
    Name         string
    Vendor       string
    Model        string
    Group        string
    Weight       uint32
    Enabled      bool
    NativeProtocol Protocol
    Modalities    []Modality

    Auth         EndpointAuth
    Routing      EndpointRouting
    Quota        EndpointQuota
    Capabilities EndpointCapabilities
}
```

repo can internally have table structs like `repo.EndpointRow` / `repo.EndpointRecord`, and then convert them into `domain.Endpoint`.

Complex JSON columns follow the same rule: business meaning goes in the domain type; SQL encoding/decoding, `Scanner` / `Valuer`, and database default-value adaptation go in the repo row type or repo-internal helpers. Don't re-expose repo types to domain just to reuse `Scan` / `Value` methods.

## 4. Middleware-owned Interfaces

Each middleware defines the minimal interface it needs itself. Interfaces live in `pkg/middleware` or in a file adjacent to that middleware, and return `domain` types.

Example:

```go
// M2 Auth
type IdentityResolver interface {
    Resolve(ctx context.Context, creds *domain.Credentials) (*domain.UserIdentity, error)
}

// M5 ModelService
type ModelCatalog interface {
    GetByModel(ctx context.Context, model string) (*domain.ModelService, error)
}

type SubscriptionChecker interface {
    HasModel(ctx context.Context, accountID string, modelServiceID int64) (bool, error)
}

// M6 RateLimit
type QuotaPolicyReader interface {
    GetQuotaPolicy(ctx context.Context, id int64) (*domain.QuotaPolicy, error)
}

// M7 Schedule
type EndpointReader interface {
    ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error)
}
```

These interfaces should not live in `pkg/repo` as an upper-layer contract. `pkg/repo` only provides the implementation.

## 5. Repo as the Implementation Layer

`pkg/repo` can contain two kinds of structs:

1. SQL row / record: carries `db` / `gorm` tags, Scanner / Valuer, close to the schema.
2. SQL reader/provider implementation: queries the database, converts rows into `domain`.

Recommended migration shape:

```text
pkg/domain/endpoint.go      the real domain.Endpoint, no db/gorm tags
pkg/repo/endpoint_row.go    endpointRow / EndpointRow, carries SQL tags and column encoding/decoding
pkg/repo/endpoint_reader.go SQL-queries rows, and returns domain.Endpoint
```

repo can use embedding to reduce duplicate fields, but shouldn't let the embedding pollute domain in reverse:

```go
type endpointRow struct {
    domain.Endpoint

    Capabilities endpointCapabilitiesJSON `db:"capabilities"`
    AuthConfig    endpointAuthJSON         `db:"auth_config"`
}
```

If embedding makes tag, zero-value, or JSON-column behavior unclear, use an explicit `ToDomain()` mapper instead. Prioritize clear boundaries over minimal code.

Example:

```go
package repo

type EndpointRow struct {
    ID      int64  `db:"id"`
    Vendor  string `db:"vendor"`
    Routing JSONRouting `db:"routing"`
    // ...
}

func (r *EndpointRow) ToDomain() *domain.Endpoint {
    return &domain.Endpoint{
        ID:     r.ID,
        Vendor: r.Vendor,
        // ...
    }
}

type SQLEndpointReader struct {
    db *sqlx.DB
}

func (r *SQLEndpointReader) ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error) {
    // query rows
    // map rows -> domain.Endpoint
}
```

Compile-time assertions can be declared in the repo package:

```go
var _ middleware.EndpointReader = (*SQLEndpointReader)(nil)
```

This expresses that repo adapts to middleware, not that middleware depends on repo.

Implementation can migrate entity by entity. It's recommended to start with `Endpoint`, since it covers routing, protocol capabilities, auth config, quota, and JSON columns all at once; once this entity is working, replicate the pattern to `UserIdentity`, `Credentials`, `ModelService`, `Secret`, `QuotaPolicy`, and other structs.

Acceptance criteria for each entity migration:

- No `type X = repo.X` in `pkg/domain`.
- `pkg/domain` does not import `pkg/repo`.
- Public return values of repo readers/providers use `domain` types.
- middleware / schedule / translator / upstream do not receive repo row types.
- `go test ./...` passes.

The dependency closure should also be checked:

```bash
go list -deps ./pkg/domain | rg '/pkg/repo$'
go list -deps ./pkg/selector | rg '/pkg/repo$'
go list -deps ./pkg/translator | rg '/pkg/repo$'
go list -deps ./pkg/invoker | rg '/pkg/repo$'
```

The goal of these commands is no output. It is fine for `pkg/repo` itself to depend on `pkg/domain`.

## 6. Middleware Options

middleware construction uses the **interface-Option pattern**, aligned with `otelgin v0.68.0`
(`opentelemetry-go-contrib/instrumentation/github.com/gin-gonic/gin/otelgin`):

- `Option` is an interface rather than a function type, to make it easy to extend with non-functional implementations in the future (stateful Options).
- The `optionFunc` adapter adapts `func(*cfg)` into an Option, so the call site's syntax doesn't change.
- middleware construction resolves all dependencies once (including the OTel `TracerProvider` /
  `Propagators`), and the closure holds the tracer; the hot path does **zero** lookups.
- Every middleware comes with `WithXxxTracerProvider(tp oteltrace.TracerProvider) XxxOption`;
  if not provided, it falls back to `otel.GetTracerProvider()`, making it easy to inject an in-memory exporter in unit tests.

Target shape:

```go
type AuthOption interface {
    apply(*authConfig)
}

type authOptionFunc func(*authConfig)

func (f authOptionFunc) apply(c *authConfig) { f(c) }

type authConfig struct {
    identity       IdentityResolver
    tracerProvider oteltrace.TracerProvider
}

func WithIdentityResolver(r IdentityResolver) AuthOption {
    return authOptionFunc(func(c *authConfig) { c.identity = r })
}

func WithAuthTracerProvider(tp oteltrace.TracerProvider) AuthOption {
    return authOptionFunc(func(c *authConfig) {
        if tp != nil {
            c.tracerProvider = tp
        }
    })
}

func Auth(opts ...AuthOption) gin.HandlerFunc {
    cfg := authConfig{}
    for _, opt := range opts {
        opt.apply(&cfg)
    }
    if cfg.identity == nil {
        panic("middleware.Auth: WithIdentityResolver required")
    }
    if cfg.tracerProvider == nil {
        cfg.tracerProvider = otel.GetTracerProvider()
    }
    tracer := cfg.tracerProvider.Tracer(ScopeName)

    return func(c *gin.Context) {
        ctx, span := tracer.Start(c.Request.Context(), "auth.lookup")
        defer span.End()
        c.Request = c.Request.WithContext(ctx)

        rc := GetRequestContext(c)
        // ... the handler calls dependencies with the local ctx (cfg.dep.Call(ctx, ...)); do not attach ctx onto RC
        _ = rc
    }
}
```

M7 / M10 / other middleware follow the same shape:

```go
type ScheduleOption interface { apply(*scheduleConfig) }

func WithEndpointReader(r EndpointReader) ScheduleOption
func WithScheduler(s selector.Scheduler) ScheduleOption
func WithSender(s *invoker.Sender) ScheduleOption
func WithScheduleTracerProvider(tp oteltrace.TracerProvider) ScheduleOption
```

Unit tests inject a stub:

```go
r.Use(middleware.Auth(
    middleware.WithIdentityResolver(fakeIdentity{}),
    middleware.WithAuthTracerProvider(testTP), // optional; noop if not provided
))
```

Option pattern rules:

- Fail fast when a required dependency is missing (panic at construction time).
- Give optional dependencies a clear default, e.g. moderator nil = pass-through, TracerProvider nil = otel global.
- The pass-through fast path (no moderator / no budget gate / no ratelimit store) directly returns
  `func(c) { c.Next() }` at construction time — **not even the tracer is turned on** — saving one
  `Tracer()` call at startup and one span Start/End per request.
- options only do wiring, no IO; the constructor must not open a DB / Redis connection (resources are
  managed by `cmd/gateway` or `pkg/server`).
- All `WithXxx*` options of the same middleware must share the same `XxxOptionFunc` adapter
  type; do not introduce a separate struct option type for a single option.

M1 `TraceContext` is the most complete reference implementation: besides `WithTraceContextTracerProvider`, it also provides
`WithTraceContextPropagators` / `WithSpanNameFormatter` / `WithTraceContextSpanStartOptions`,
fully mirroring otelgin's `WithPropagators` / `WithSpanNameFormatter` / `WithSpanStartOptions`.

## 7. Redis

Redis carries two kinds of shared state:

1. **Rate limit buckets**: the `ratelimit.RedisStore` implementation does pre-deduction for user-side RPM/RPS, post-deduction for TPM, and quota reserve / charge after an endpoint is selected.
2. **Cooldown**: `selector.NewRedisCooldownManager` records the short-term isolation state of failed endpoints.
   The `CooldownManager` interface is `Mark(ctx, endpointID, class, retryAfter)` / `InCooldown(ctx, ids)` /
   `Clear(ctx, endpointID)` — `Mark`'s `retryAfter` carries the upstream's own recovery hint for reset-aware
   TTLs (docs/03 §9), and `Clear` backs the health prober's probe-gated early release (docs/03 §10).

There is no in-memory Store as a gateway production fallback. Across multiple replicas, both rate limiting and cooldown must share Redis.

Unit tests can use a fake store / fake CooldownManager, but only within the test package, never as a production driver.

## 8. Repo Cache: deployer SQL → gateway Data Propagation

`cmd/migrate` applies versioned schema changes; business rows are maintained by SQL or the optional console.
The gateway's data plane is **100% read-only** against MySQL — no INSERT / UPDATE / DELETE.
The propagation bridge between the two sides doesn't need a real-time invalidation channel (Debezium /
outbox table, etc.) — it's enough for the repo layer to fall back on an in-process
TTL LRU cache:

```text
deployer --SQL INSERT/UPDATE--> MySQL
                                  │
                            (request)│
                                    ▼
                  gateway: repo.CachedXxxReader (TTL LRU, default 30s)
                                    │ miss
                                    ▼
                  L3: direct MySQL query (repo.SQLXxxReader.Get*)
```

### 8.1 Components

| Layer | Role | Implementation |
|----|------|------|
| MySQL | source of truth | `pkg/infra/schema.sql` |
| `repo.TTLCache[K, V]` | in-process LRU + TTL; does not cache not-found | `pkg/repo/cache.go` |
| `repo.CachedXxxReader` | cached wrapper for the 5 SQL Readers/Providers | `pkg/repo/cached.go` |

### 8.2 Applicable Tables and Default Parameters

| Cached Wrapper | Wrapped SQL Reader | cache key | default cap / ttl |
|---|---|---|---|
| `CachedAPIKeyProvider` | `SQLAPIKeyProvider` | `HashAPIKey(plain)` | 10240 / 30s |
| `CachedModelServiceReader` | `SQLModelServiceReader` | `model` | 256 / 30s |
| `CachedEndpointReader` | `SQLEndpointReader` | `"model\x00group"` + `id` | 1024+4096 / 30s |
| `CachedQuotaPolicyProvider` | `SQLQuotaPolicyProvider` | `id` | 128 / 30s |
| `CachedSubscriptionProvider` | `SQLSubscriptionProvider` | `accountID\x00modelServiceID` | 10240 / 30s |

### 8.3 Invalidation Semantics

**TTL by default, targeted invalidation for API keys** — most records rely on natural TTL expiry. Console can publish best-effort cachebus events for API-key revocation; TTL remains the fallback.

- Endpoint, quota and pricing changes normally accept the bounded TTL window.
- API-key revocation can propagate sub-second through cachebus when console is configured with Redis.

### 8.4 Not Caching "not found"

When the loader returns nil/zero, it **does not** Set — this avoids a negative cache getting stuck on
"a resource that was just created", letting newly added data get a miss within the TTL window and go
back to the source (worst case, one L3 trip per request, and it hits on the next Set).
Sole exception: `CachedSubscriptionProvider` caches `false` (a missing subscription is a common path,
and going back to the source is expensive).

### 8.5 What's Not Done

- **No L2 Redis shared cache**: each gateway replica maintains its own in-process cache; on L1 miss
  it goes straight to L3 MySQL. Simple, and there's no cross-replica consistency issue.
- **No CDC / binlog listening**: the data plane is 100% read-only, so it doesn't need a push-based
  invalidation channel; TTL is already sufficient.
- **No stale-while-revalidate / refresh-ahead**: on TTL expiry it's evicted directly, and the next
  Get goes back to the source on miss. If async refresh is ever needed, decide based on metrics.

## 9. BudgetGate

M4 Budget is replaceable:

- `alwayspass`: default, always passes.
- `inmemory`: in-process balance tracking, suitable for demo/single-instance.

**`inmemory` must never be used with multiple replicas**: the balance is per-process — N replicas
each deduct independently, effectively granting N times the budget, and it resets to zero on a
rolling restart. Multi-replica deployments should either use `alwayspass` (budget managed by the
downstream billing system) or implement an external accounting `BudgetGate` (shared storage).

When adding a new external accounting system, implement middleware's `BudgetGate` interface, and inject it via an option in `cmd/gateway`.

## 10. Moderation

M8 Moderation is replaceable:

- `none`: default, skips moderation.
- `openai`: calls the OpenAI moderation API, requires `moderation.api_key`.

When the moderator returned is nil, M8 is pass-through.

## 11. Recording / Usage Events

Usage events are selected via `usage_events.driver`, four mutually exclusive drivers (implementation goes through the Outbox Pattern, `pkg/usage.OutboxPublisher` interface):

- `file`: local JSONL append; suitable only for local development or ad-hoc troubleshooting.
- `kafka`: synchronous Kafka producer; only returns once publishing completes — higher latency, no local copy.
- `async_kafka`: async buffer + retry + backoff + DLQ topic; can survive brief broker jitter.
- `file_and_kafka`: **recommended for production** — Transactional Outbox Pattern; file is the source of
  truth (sync commit), Kafka is the async broadcast (best-effort, reuses AsyncKafkaOutbox).
  Still able to commit if the broker is down; an external replay tool reads the file to re-publish.

See [07-configuration §2 `usage_events`](./07-configuration.md#2-gatewayyaml) for the full config schema, and [05-metering-billing §5](./05-metering-billing.md#5-usage-outbox) for failure semantics.

Content Log is a separate channel, and does not reuse the Usage Event schema. A content recorder can be wired up via `upstream.WithHooks(...)`.

`async_kafka`'s buffer, max retries, backoff, and DLQ topic are declared in the `usage_events.kafka.*` config block (`file_and_kafka` reuses these fields to configure the Kafka side). Producer shutdown is centrally managed by `pkg/server` (see §12 graceful shutdown order).

## 12. Tracing

trace driver:

- `slog`: default, structured logging.
- `otel`: initializes the OTLP provider, and calls `Shutdown` via the server closer on exit.

`trace.CtxHandler` wraps the slog JSON handler, so that `slog.InfoContext` / `ErrorContext` automatically carry trace_id, span_id, and baggage.

Calling context-less methods like `slog.Info` / `slog.Error` / `slog.Warn` directly on the request path is forbidden. Implementations should add a lint or test scan to make sure all logging entry points use `slog.*Context`.

**Middleware OTel integration mirrors otelgin v0.68.0**: all middleware build
`tracer := cfg.tracerProvider.Tracer(ScopeName)` once at construction time, and the closure holds it. M1 TraceContext
is the complete reference (`WithTraceContextTracerProvider` / `WithTraceContextPropagators` /
`WithSpanNameFormatter` / `WithTraceContextSpanStartOptions`, expanded in §6); other
middleware only provide `WithXxxTracerProvider` (defaulting to OTel global), and span names use
fixed verbs directly (`auth.lookup` / `catalog.resolve` / `ratelimit.reserve` / `schedule.pick`
/ `moderation.check` / `tracing.commit`).

OTel attribute naming prioritizes the OpenTelemetry `gen_ai.*` / HTTP semconv standards; when
standard fields are missing, use the `llm_gateway.*` prefix. The full attribute list and recommended
span structure are maintained in [08-observability §4 Tracing](./08-observability.md#4-tracing) and not
repeated here; metric naming and dimensions are in [08-observability §3 Metrics](./08-observability.md#3-metrics).

## 13. Server Lifecycle

`internal/app/runtime.Runtime` is responsible for:

- Opening the DB / Redis / Kafka producer.
- Registering closers.
- Serve.
- Catching SIGTERM/SIGINT.
- Graceful shutdown.
- Closing resources in reverse order.

If `cmd/gateway`'s `buildEngine` fails midway, it defers `srv.Close()` to clean up already-opened resources.

Liveness / readiness:

- `/healthz` is liveness — it only indicates the process's event loop can still respond, and does not depend on SQL / Redis / Kafka.
- `/readyz` is readiness — checks that SQL and Redis are reachable; it does not check Kafka / outbox, because a usage publish failure should not cause the gateway to be pulled out of traffic.
- If readiness keeps failing past a configured threshold, liveness can be made to return failure too, to avoid a pod staying not-ready indefinitely.

Graceful shutdown order:

1. Upon receiving SIGTERM/SIGINT, the HTTP server stops accepting new requests.
2. Wait for in-flight requests to finish, bounded by `server.shutdown_timeout`, default 30s.
3. Flush and close the `async_kafka` producer / outbox.
4. Close the Redis client.
5. Close the DB pool.

In-flight requests that exceed the shutdown timeout get interrupted, and this is recorded as `llm_gateway_request_aborted_by_shutdown_total`. The shutdown order must not close Kafka/Redis/DB before waiting for requests to finish, otherwise M6's post-side, M10's outbox, and tracing wrap-up would lose their dependencies.

## 14. Admin Boundary

`cmd/migrate` owns schema changes; business rows are maintained by deployer SQL or console. The following tables are within that maintenance scope:

- accounts
- api_keys
- quota_policies
- model_services
- account_model_subscriptions
- endpoints
- pricing_versions

gateway is read-only against these tables, except for audit-type fields such as API key last-used, if implemented, which should be called out separately.

## 15. Evolution Rules

- Adding an import of, or a type alias to, `pkg/repo` in `pkg/domain` is forbidden.
- For a new middleware, first define the minimal interface in the middleware package, then have repo
  implement it; Options use the interface + optionFunc shape, aligned with otelgin v0.68.0 (§6).
- repo returns domain structs — repo row types must not be leaked to middleware.
- When adding a new infra driver, register it consistently in config, the cmd build function, example
  config, and this document.
- When required startup dependencies change, [00-overview](00-overview.md)'s startup flow must be updated.
- Do not claim "zero external dependency startup" in the docs, unless the code actually provides a
  runnable DB/Redis substitute implementation.
- Adding a new repo cached wrapper (§8.2) must be synced together with: defining the cache key, the
  default cap/ttl, and this document's §8.2 table.

## 16. Known Limitations (security/tech debt — read before making changes)

- **API key hash is unsalted single-round SHA-256** (`repo.HashAPIKey`): if the api_keys table leaks,
  weak keys can be brute-forced offline / the same key can be correlated across accounts. gateway does
  not generate keys itself (deployer inserts the hash), so it cannot guarantee key entropy. Mitigation:
  deployer generates ≥256-bit random keys. Root-cause direction: HMAC-SHA256(pepper, key) — needs a
  dual-read migration (query both old hash and new hash for a period), not yet scheduled.
- **data_key (KEK) has no rotation path**: ciphertext carries a `v1:` prefix but there's no v2 decryption
  chain — if the KEK is suspected leaked, the only option is to stop the service and manually
  decrypt/re-encrypt all of endpoints.auth. Root-cause direction: `SetDataKeys(new, old...)` multi-key
  decryption + background re-encrypt, not yet scheduled.
- **Rate limiting is incompatible with Redis Cluster**: see [04 §7a](./04-rate-limiting.md#7a-redis-deployment-shape-limitations).
- **OTel Baggage must not be injected into upstream requests**: internal tenant identifiers live in
  baggage; the upstream client is only allowed to inject traceparent (see the comment in
  `pkg/trace/otel.go`).
