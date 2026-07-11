# 00 — Overview

## 1. Project Goals

`llm-gateway` is an LLM request gateway that provides a unified client entry point, forwarding requests to upstream vendors or self-hosted model services according to the primary account, model, group, and endpoint configuration.

The target architecture focuses on solving:

1. **Multi-protocol entry and upstream protocol conversion**: OpenAI Chat / Responses, Anthropic Messages, and other entry points all flow into the same routing pipeline, then get converted to the upstream's native protocol by the translator.
2. **Primary account control**: the API key resolves into the primary account's pin, sub-account/operator, group, and quota policy; model visibility is controlled by the subscription table.
3. **Endpoint selection and retry**: candidates are pulled by model + group, filtered first for protocol/modality eligibility, then endpoints are swapped within the same model; cross-model fallback only executes when explicitly declared via header.
4. **Redis rate limiting**: a two-layer quota policy (primary account + API key); RPM/RPS are reserved before the request, TPM is deducted after based on actual usage; endpoint quota is only deducted for the finally selected endpoint.
5. **Logging and metering output**: content logging, Usage Events, and Metrics/Trace are three independent channels; the gateway only produces factual data — pricing is done downstream.
6. **Observability**: slog trace fields, Prometheus metrics, optional OpenTelemetry tracer.

## 2. Non-goals

- Does not implement the model inference service itself.
- Does not implement RAG, prompt orchestration, agent, or business BFF logic.
- Business tables are maintained directly via SQL by the deployer; a separate `cmd/console` control-plane binary (Admin API) is also available to manage them, but the data plane never depends on it.
- Does not do billing aggregation inside the gateway process; the gateway only emits usage events, and pricing aggregation is done by a downstream job.

## 3. Running Processes

| Process | Entry point | Responsibility |
|------|------|------|
| migrate | `cmd/migrate` | Applies versioned schema migrations before a production rollout |
| gateway | `cmd/gateway` | Thin data-plane entry; assembly lives in `internal/app/gateway`; validates schema version at startup |
| console | `cmd/console` | Control plane: an Admin API (backed by `pkg/console`) for managing business data; optional, and the data plane runs without it |

Business data (accounts / api_keys / model_services / endpoints / quota_policies /
subscriptions / pricing_versions) is maintained directly via SQL INSERT/UPDATE/DELETE by the deployer;
the separate `cmd/console` control-plane binary is an additional way to manage it (it can also publish
cachebus invalidation that the gateway subscribes to for fast revocation).

Gateway startup sequence:

1. Load `configs/*/gateway.yaml`.
2. Initialize the endpoint auth encryption data key.
3. Open the **SQL DB (required)**, verify that `cmd/migrate` has applied the latest schema version,
   then run `repo.CheckSchema`; local development may explicitly enable `database.auto_migrate`.
4. Open **Redis (required)**; M6 rate limiting and scheduler cooldown both depend on Redis.
5. Assemble SQL reader/provider (wrapped in a `repo.CachedXxxReader` TTL LRU layer, see
   [06 §8](./06-pluggable-infra.md#8-repo-cache-deployer-sql--gateway-data-propagation)),
   Redis rate limit store, scheduler, outbox, tracer.
6. Scan enabled endpoints, validate that the vendor adapter exists, `endpoint.Protocol` is valid, and the required translator is registered; if missing, only log a warning and emit a metric, without blocking startup.
7. Call `router.NewEngine` to register routes and middleware.
8. Hand off to `pkg/server` for listen, signal handling, graceful shutdown, and reverse-order resource teardown.

Deployment order:

1. docker stack brings up MySQL + Redis (+ Kafka/Redpanda if using the kafka outbox driver).
2. Run `cmd/migrate` once, or let the Helm migration Job apply pending versions.
3. Start gateway, then manage business data through SQL or the optional console.
4. The deployer SQL-INSERTs business data such as accounts, API keys, model services, subscriptions, endpoints,
   and quota policies (the endpoint.auth column must be encrypted with `repo.EncodePayload`,
   and the api_keys.api_key_hash column must be computed with `repo.HashAPIKey` as a SHA-256 hex).

SQL → gateway data propagation goes through the **repo layer's in-process TTL LRU cache** (default 30s): the deployer writes to
MySQL → once the gateway repo cache naturally expires, a miss triggers a direct SQL lookup to fetch the new value. **There is no direct per-request
query against the same DB**. Most records rely on TTL expiry. API-key revocation is the deliberate
exception: when console cachebus is enabled, it publishes a best-effort targeted invalidation; TTL remains the fallback. See
[06 §8](./06-pluggable-infra.md#8-repo-cache-deployer-sql--gateway-data-propagation) for details.

Schema changes go through the versioned migrations in `pkg/infra`, applied by `cmd/migrate`.
Changes must remain backward compatible: first deploy a new gateway with the new schema, letting it create the new tables/columns (keeping old fields);
only after all gateways have finished upgrading should you drop fields or make breaking changes.

## 4. Request Lifecycle

```text
HTTP request
  |
  | pre: BodyLimit / Timeout
  v
M1 TraceContext      Generates RequestID, injects OTel SpanContext/Baggage, creates RequestContext
M9 Recover           defer-based fallback for panics and unified error responses
M2 Auth              Parses API key/JWT into domain.UserIdentity
M3 Envelope          Reads the raw body, extracts model, records source protocol + modality
M4 Budget            alwayspass or inmemory gate; failure aborts immediately
M5 ModelService      Looks up the global model catalog and the primary account's subscription
M8 Moderation        Optional content moderation; defaults to none
M6 Limit             Pre-deducts the user-side RPM/RPS, post-deducts TPM based on usage after the response
M7 Schedule          Pulls endpoint candidates, schedules, retries, forwards upstream, writes Usage/Decision/Error
M10 Tracing          metric, usage meta, outbox, scheduling trace
  |
  v
HTTP response
```

Note: M6 uses gin's onion model, executing the user-side TPM `ChargeBatch` after `c.Next()`; this is not done in M10.

## 5. Component Layering

```text
cmd/gateway
  -> config.Load
  -> server.OpenDB/OpenRedis/NewKafkaProducer
  -> repo SQL readers/providers
  -> router.NewEngine

pkg/router
  -> Registers the full /v1/... routes per modality
  -> Composes the middleware chain

pkg/middleware
  -> Request lifecycle, RequestContext read/write, error abort
  -> Each middleware has its own custom Option (interface) + WithXxxTracerProvider, aligned with otelgin v0.68.0

pkg/repo (cached)
  -> SQL Reader/Provider wrapped in a TTL LRU layer (repo.TTLCache[K,V] + CachedXxx wrapper)
  -> ModelCatalog / EndpointReader / APIKeyProvider and other middleware-owned readers use the cached version directly

pkg/selector
  -> Performs filter / pick / report over a batch of candidate endpoints; holds no repo, does not switch fallback model

pkg/invoker
  -> Handler lookup, HTTP Do, response forward

pkg/protocol
  -> Handler facade (PrepareCall → NewResponseStream)
  -> Factory / Session: vendor HTTP layer (URL, auth header, request construction)
  -> protocol/quirks: endpoint-level body/header tweak DSL (rename / strip / set / set_default)

pkg/translator
  -> Request/response conversion between the client protocol and the upstream protocol, usage extraction

pkg/repo + pkg/infra
  -> SQL schema, CRUD/readers, Redis, Kafka
```

## 6. Key Terminology

| Term | Intended meaning |
|------|----------|
| pin | The account's stable external identifier, used as the billing subject ID; distinct from the database auto-increment primary key, assigned by the deployer when the account is created, and immutable thereafter |
| Group | A request grouping dimension under the primary account, affecting endpoint candidate filtering; defaults to `default`, can be used for isolation scenarios such as `reserved` / `experimental` |
| `RequestContext` | The state object for a single request, attached to `c.Request.Context()`, retrieved via a middleware helper |
| `RequestEnvelope` | M3's output: raw body, model, source protocol, modality; no longer contains the canonical request |
| `UserIdentity` | M2's output, containing the primary account's pin, sub-account/operator, API key, group, and quota policy IDs |
| `ModelService` | Global model catalog record; whether the primary account can use it is determined by the subscription table |
| `Endpoint` | A global upstream access point, matched by model + group; includes vendor, weight, auth, routing, quota, capabilities |
| `Adapter` | The vendor's HTTP-layer factory/session; not responsible for protocol conversion or usage aggregation |
| `Translator` | The protocol conversion layer; responsible for request body conversion, response handler, usage extraction |
| `Scheduler` | M7's within-batch endpoint selector, exposing Pick/Report, not responsible for cross-model fallback |
| `RateLimitState` | Rate-limit state written by M6/M7, used for TPM post-deduction and troubleshooting; not a client header contract |
| `Usage` | Resource consumption and metadata for a single request, published by M10 to the outbox |
| `TTLCache` | The gateway's in-process LRU + TTL cache (`pkg/repo/cache.go`), the repo's sole caching strategy |
| `CachedXxxReader` | The repo layer's cached wrapper, wrapping a SQL Reader with a TTL LRU layer; default 30s |

## 7. Document Version

| Version | Date | Notes |
|------|------|------|
| v0.3-target | 2026-05-16 | Aligned target boundaries: protocol capabilities pushed down to endpoint, simplified scheduler, RPM/RPS pre-deduction, TPM post-deduction, downstream billing |
| v0.4-target | 2026-05-17 | middleware Option aligned with otelgin v0.68.0; domain/repo fully decoupled |
| v0.6-target | 2026-05-21 | Removed `cmd/admin` + Flink + Debezium CDC: the data plane is 100% read-only MySQL, repo uses TTL LRU cache instead of real-time invalidation |
| v0.7-target | 2026-05-22 | Merged `pkg/adapter` into `pkg/protocol` (vendor Factory / Session / Classifier all live in the protocol package); endpoint.quirks JSON column + DSL; dispatch / repo cache wired into OTel & Prom |
