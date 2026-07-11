# 01 — Request Pipeline

This document records the request chain for `pkg/router` + `pkg/middleware`, along with `internal/requeststate.State`.

## 1. Route Assembly

`pkg/router.NewEngine` creates the `gin.Engine` and registers:

- ops routes: `/healthz`, `/readyz`, `/metrics`, etc., maintained in `helpers.go`.
- chat: `/v1/chat/completions`, `/v1/messages`, `/v1/responses`.
- image: `/v1/images/{generations,edits,variations}`.
- audio: `/v1/audio/{speech,transcriptions,translations}`.
- embedding: `/v1/embeddings`.

Each route file declares its own complete `/v1/...` path; a global `/v1` group is not used. Each modality file explicitly lists its own middleware chain, avoiding a shared helper that would tie different modalities together.

## 2. RequestContext Storage

`RequestContext` is attached to `c.Request.Context()` via `context.WithValue`, rather than `gin.Context.Set/Get`.

Entry helpers:

- `middleware.AttachRequestContext(c, rc)`: called only by M1.
- `middleware.GetRequestContext(c)`: panics if not found, backstopped by M9 Recover.

This way request state, the OTel SpanContext, and Baggage are all carried in the same stdlib context container.

## 3. `internal/requeststate.State`

Target definition:

```go
type State struct {
    RequestID string
    StartTime time.Time

    Identity UserIdentity
    Envelope *RequestEnvelope
    Handlers protocol.Lookup

    ModelService *ModelService // model from the original request
    ModelChain   []*ModelService // sequence of attempts pre-resolved by M5: primary + validated fallbacks
    RoutedModelService *ModelService // the model that actually succeeded; equals ModelService when no fallback occurred

    RateLimit *RateLimitState

    Endpoint *Endpoint

    Usage              *Usage
    Error              *AdapterError
    SchedulingDecision *SchedulingDecision
}
```

Important constraints:

- `trace_id` / `span_id` are not stored as fields; they are extracted from the OTel context inside `c.Request.Context()` (`middleware.TraceIDFromCtx`).
- **`context.Context` is not attached to RC** — the single source of truth is `c.Request.Context()`. Middleware reads ctx via `c.Request.Context()` and writes it back via `c.Request = c.Request.WithContext(ctx)`. Attaching a ctx field to a mutable struct violates Go's "context is values, not state" principle, and would drift from gin's native `c.Request.Context()`.
- `*gin.Context` is not stored; response writing is done by middleware using the current handler's `c.Writer`.
- Untyped extension maps are forbidden; new state must have an explicit type and owner.
- `*slog.Logger` is not stored; logging uses `slog.*Context` methods, and `trace.CtxHandler` automatically fills in the trace/baggage fields.
- Business code must use context-carrying methods such as `slog.InfoContext` / `ErrorContext` / `WarnContext`; calling `slog.Info` / `Error` directly in the request path is forbidden, otherwise trace fields cannot be injected.
- M4 Budget does not write `BudgetStatus` to RC; it either passes through and continues, or fails and aborts.
- The adapter session is not attached to RC; only the response-stage artifacts `Usage`, `Error`, and `SchedulingDecision` are retained.

## 4. Middleware Chain

| No. | Name | Main Input | Main Output/Side Effect |
|------|------|----------|-----------------|
| pre | BodyLimit | config `middleware.body_limit_bytes` | returns 413 when exceeded |
| pre | Timeout | config `middleware.timeout` | sets a timeout on the request context |
| M1 | TraceContext | HTTP headers/context | creates RC, RequestID, span/baggage |
| M9 | Recover | RC/Error/panic | unified error JSON, panic backstop |
| M2 | Auth | Authorization / X-API-Key | `rc.Identity` |
| M3 | Envelope | request body + route protocol tag | `rc.Envelope`, body can be re-read |
| M4 | Budget | `rc.Identity` | abort on gate failure |
| M5 | ModelService | `rc.Envelope.Model`, primary account pin, `X-Gateway-Fallback-Models` | `rc.ModelService` (primary), `rc.ModelChain` (primary + validated fallbacks) |
| M8 | Moderation | raw request/response stream | optional moderation, defaults to none |
| M6 | Limit | identity, model, quota policy | user-side RPM/RPS pre-deduction, `rc.RateLimit`; post-side TPM post-deduction |
| — | Cache | request body / prompt | response cache (chat + embedding modalities only); on a hit returns directly and skips M7; a no-op when `cache.enabled=false` |
| M7 | Schedule | model, group, endpoint candidates | `rc.RoutedModelService`, endpoint, upstream forward, usage, decision |
| M10 | Tracing | final RC state | metric, usage outbox, schedule trace |

M9 is registered early, but via defer it covers panics from all middleware and handlers after M2; pre middleware (BodyLimit / Timeout) must either not panic themselves or backstop their own panics.

**M10 is registered after M1 and before M9** (its finishing logic runs in the post-`c.Next()` onion return phase):
- If any subsequent middleware aborts (401/429/503) → the finishing logic still runs on the return trip — request metrics /
  usage events / decision audit have **no blind spots** (an older version registered at the tail of the chain would be
  entirely skipped by `c.Abort()`, letting things like credential-stuffing / rate-limit storms hide invisibly in the
  request metrics)
- On panic → the inner M9 recovers first and writes a 500, control flow returns normally, and M10's finishing logic sees the final 500 status

## 5. M2 Auth

The target dependency is declared by M2 itself as a minimal interface, with repo serving only as the implementation. API keys are stored as SHA-256 hashes; after parsing they yield a `domain.UserIdentity`:

```go
type UserIdentity struct {
    AccountID            string // primary account pin / billing entity
    SubAccountID         string // sub-account / operator
    APIKeyID             string
    Group                string
    ExternalUser         bool
    AccountQuotaPolicyID *int64 // primary-account-level rate limit policy
    APIKeyQuotaPolicyID *int64 // API-key-level rate limit policy
}
```

`AccountID` is the primary account pin / billing entity; `SubAccountID` is the sub-account / operator under that primary account. `AccountQuotaPolicyID` comes from the `accounts` join, `APIKeyQuotaPolicyID` comes from `api_keys`. M6 uses these two IDs to avoid repeatedly looking up the primary account's and key's policy relationships on every request.

API key lookup is based on the unique index on `key_hash`; once the hash hits, `AccountID`, `SubAccountID`, and the policy IDs are joined out from accounts / api_keys — the caller cannot alter ownership via a header. `SubAccountID` is associated via the API key record, and is not trusted directly from a request header. A theoretical hash collision is treated as a system error, returning 500/503 with an alert.

Once M2 has finished resolving credentials, subsequent logs, trace baggage, upstream forwarding, and Content Log must not carry the original `Authorization` / `X-API-Key` values; when upstream authentication needs to be passed through, the header is regenerated solely from the endpoint auth configuration.

## 6. M3 Envelope

`RequestEnvelope` only carries the minimal information needed for routing:

```go
type RequestEnvelope struct {
    RawBytes       []byte
    Model          string
    SourceProtocol Protocol
    Modality       Modality
}
```

M3 does not perform canonicalization and does not parse the full set of protocol fields. Request body shape conversion is handled by `pkg/translator`.

## 7. M5 ModelService

M5 uses middleware-owned interfaces, injected by cached wrappers from `pkg/repo`:

- `ModelCatalog.GetByModel` looks up the global model catalog (production implementation: `repo.CachedModelServiceReader`
  returns directly on a TTL LRU hit; on a miss it falls through to `repo.SQLModelServiceReader.GetByModel`; TTL defaults to 30s).
- `SubscriptionChecker.HasModel` checks whether the primary account is subscribed to that model (also via a cached wrapper).

`model_services` is a global catalog and is no longer stored per primary account; visibility per primary account is determined by the `account_model_subscriptions` table.

The list of the 5 cached wrappers + their default parameters is in [06 §8.2](./06-pluggable-infra.md#82-applicable-tables-and-default-parameters).

M5 does not query active pricing, nor does it write a pricing snapshot to `RequestContext`. Price matching and amount calculation are done by the downstream billing platform based on the request occurrence time in the Usage Event.

Cross-model fallback validation is also done in M5: it parses the `X-Gateway-Fallback-Models` header (see [03 §5](./03-endpoint-scheduling.md#5-retry-model) for details), and for each fallback model re-runs the catalog + subscription checks, writing the validated `*ModelService` entries into `rc.ModelChain` (primary at `[0]`, fallbacks appended in order). A fallback that doesn't exist or isn't subscribed is silently dropped — as long as the primary succeeds, the request proceeds. The M7 thin adapter projects `rc.ModelChain` into `dispatch.Input`, which is consumed by `dispatch.Dispatcher` to decide whether to switch to a fallback model; M7 only writes the `dispatch.Outcome` back into `rc.RoutedModelService` / `rc.Usage` / `rc.SchedulingDecision`. Both the usage meta and the attempt record use the actually routed model.

SQL query failures are uniformly treated as dependency failures: when M2 IdentityResolver, M5 ModelCatalog, SubscriptionChecker, or the dispatch `CandidateSource` (bridged to repo EndpointReader in production) return a DB error, they fail closed, responding with 503 and `Retry-After`, and must not be disguised as 401/403/404. The PolicyCache's explicit TTL cache is an exception; on a cache hit, the cached value may continue to be used, but after a cache miss a DB failure still returns 503.

## 8. Error Exit

Early middleware writes `rc.Error` and calls `c.Abort()` via the internal `abort(c, status, class, message)`. M9 Recover uniformly converts `rc.Error` into the response.

Once the M7 response stream has started, subsequent errors can no longer overwrite the HTTP status; at that point only `rc.Error` is written, for logging, metrics, and outbox reference.

The unified error response body is wrapped in a top-level `error` field, to make it easy to extend with non-error return fields in the future:

```go
type ErrorResponse struct {
    Error ErrorBody `json:"error"`
}

type ErrorBody struct {
    Code      string         `json:"code"`
    Message   string         `json:"message"`
    Class     string         `json:"class"`
    Details   map[string]any `json:"details,omitempty"`
    RequestID string         `json:"request_id"`
    TraceID   string         `json:"trace_id"`
}
```

Constraints:

- `Code` is a stable, machine-readable error code, e.g. `rate_limit_exceeded`, `invalid_request`, `no_endpoint_available`.
- `Message` is a human-readable description and must not be used as a basis for program logic.
- `Class` aligns with the scheduling/upstream error classification, e.g. `invalid`, `rate_limit`, `transient`.
- `Details` should only contain fields necessary for troubleshooting, e.g. the rate-limit dimension, bucket key, endpoint id; it must not contain the request / response body.
- `RequestID` / `TraceID` are populated by M9 Recover from `rc`; clients can use these to correlate with logs / traces without needing a separate header.

See [08-observability §7](./08-observability.md#7-error-response) for a JSON example.

## 9. Framework Boundary

`c.Next()` as it appears in this document refers to the post-handler phase of gin's onion model; the target contract is "pre-processing -> downstream handler/middleware -> post-processing finishing", not a requirement that business logic depend on the gin API. If the HTTP framework is ever replaced, this execution semantics must be preserved.

## 10. Middleware Assembly Contract (aligned with otelgin v0.68.0)

Each middleware exposes:

- an `XxxOption` interface + a private `xxxOptionFunc` adapter (no longer the function-type
  `func(*cfg)`).
- a `WithXxxFoo(...)` constructor for required dependencies, panicking at construction time if missing.
- `WithXxxBar(...)` for optional dependencies, with a clear default when nil (e.g. a nil moderator = pass-through).
- `WithXxxTracerProvider(tp oteltrace.TracerProvider)`: injects the OTel TracerProvider;
  falls back to `otel.GetTracerProvider()` at startup when nil.

One-time setup at middleware startup:

```go
cfg := xxxConfig{}
for _, opt := range opts { opt.apply(&cfg) }
if cfg.required == nil { panic("middleware.Xxx: WithXxxRequired required") }
if cfg.tracerProvider == nil { cfg.tracerProvider = otel.GetTracerProvider() }
tracer := cfg.tracerProvider.Tracer(ScopeName)   // held by closure
return func(c *gin.Context) { /* hot path */ }
```

Standard hot-path template (the single source of truth for ctx propagation is `c.Request.Context()`, **not** an RC field):

```go
return func(c *gin.Context) {
    ctx, span := tracer.Start(c.Request.Context(), "xxx.action")
    defer span.End()
    c.Request = c.Request.WithContext(ctx)   // ← downstream mw automatically picks up the span

    rc := GetRequestContext(c)               // RC only carries data, not ctx
    // ... all business calls pass the local ctx: cfg.dep.Call(ctx, ...)
}
```

Pass-through fast paths (pass-through moderator / no budget gate / no ratelimit
store) return `func(c) { c.Next() }` at startup time already, without even opening a tracer. See
[06 §6 Middleware Options](./06-pluggable-infra.md#6-middleware-options) for details.

M1 TraceContext is the reference implementation, additionally exposing `WithTraceContextPropagators` /
`WithSpanNameFormatter` / `WithTraceContextSpanStartOptions`, fully mirroring otelgin's
`WithPropagators` / `WithSpanNameFormatter` / `WithSpanStartOptions`.

## 11. Evolution Rules

- When adding a new RC field, the writing middleware and the readers must be documented. Fields related to cross-model fallback must distinguish between the original request model and the actually routed model.
- When adding a new middleware, update the ordering table in this document, and check whether all modality routes need to adopt it.
- Request-path logging should have lint/test constraints added, scanning for non-Context calls such as `slog.Info(`, `slog.Error(`, `slog.Warn(`.
- Do not add temporary untyped fields to request state; promote only stable, typed state with clear ownership.
