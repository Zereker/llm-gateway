# Architecture

This directory is the single source of truth for `llm-gateway`'s architecture and interface contracts. The code implements what is documented here; if an implementation needs to change the boundaries described here, update this directory first, then change the code.

## Document Index

| # | Document | Topic |
|---|------|------|
| 00 | [overview](00-overview.md) | System boundaries, component layering, request lifecycle |
| 01 | [request-pipeline](01-request-pipeline.md) | `internal/requeststate.State` and the middleware chain |
| 02 | [protocol-translation](02-protocol-translation.md) | Handler facade, Factory / Translator / Quirks, upstream forwarding boundary |
| 03 | [endpoint-scheduling](03-endpoint-scheduling.md) | Endpoint candidates, in-batch selection, explicit fallback, runtime scoring |
| 03a | [schedule-overview](03a-schedule-overview.md) | Quick reference / onboarding guide for the schedule module (data flow, package responsibilities, wiring points) |
| 04 | [rate-limiting](04-rate-limiting.md) | User-side RPM/RPS pre-deduction, TPM post-deduction, endpoint quota |
| 05 | [metering-billing](05-metering-billing.md) | Content logging, usage events, metrics / trace, downstream billing boundary |
| 06 | [pluggable-infra](06-pluggable-infra.md) | Injection points for DB, Redis, Kafka, OTel, budget, and moderation |
| 07 | [configuration](07-configuration.md) | Gateway config schema, environment variable overrides, validation rules |
| 08 | [observability](08-observability.md) | Logging, metrics, trace, usage events, content log observability contract |

## Architecture Highlights

- The data plane is `cmd/gateway`; a separate `cmd/console` control-plane binary (Admin API, backed by `internal/console`) is also available. Business data is managed via direct SQL, and the console is an optional additional way to manage it — the data plane never depends on it.
- Catalog, endpoint, API key, subscription, and quota policy data live in SQL; `cmd/migrate` applies versioned schema changes and gateway startup performs read-only version/schema checks.
- The gateway depends on Redis for M6 rate limiting and scheduler cooldowns.
- SQL writes propagate to the gateway through the [repo in-process TTL LRU cache](./06-pluggable-infra.md#8-repo-cache-deployer-sql--gateway-data-propagation) (default 30s) rather than querying the same tables per request; the data plane is 100% read-only, so the TTL window is sufficient.
- Client-facing entry points cover OpenAI Chat, Anthropic Messages, OpenAI Responses, Images, Audio, and Embeddings routes; Gemini is supported as an upstream protocol only and is not exposed as a client entry point.
- The `internal/protocol` package is both the Handler facade and the home of vendor Factory / Session implementations (the HTTP-layer factories) plus the endpoint-level quirks DSL; protocol shape translation lives in `internal/translator`, and usage extraction lives in `internal/usage`. Consumers only see `protocol.Handler` / `protocol.Lookup` and never type-assert Factory.
- All middleware wiring uses the interface-Option pattern (aligned with otelgin v0.68.0); see [06 §6](./06-pluggable-infra.md#6-middleware-options) and [01 §10](./01-request-pipeline.md#10-middleware-assembly-contract-aligned-with-otelgin-v0680).

## Maintenance Conventions

- When changing `internal/requeststate.State`, the middleware order, adapter/translator interfaces, schema, or config fields, update this directory in the same change.
- Example code illustrates the key contracts only; it need not match the implementation verbatim, but field names, component boundaries, and error semantics must be accurate.
- When adding a new cached repo wrapper, middleware Option, or metric, register it in the corresponding section of 06 / 07 / 08.
