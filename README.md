# llm-gateway

[English](README.md) | [简体中文](README.zh-CN.md)

A Go-based gateway that routes LLM API requests to multiple upstream providers
(OpenAI, Anthropic, Google, AWS Bedrock, vLLM / Ollama self-hosted, etc.) under
one OpenAI-compatible interface.

## Status

A Go implementation of an LLM API gateway. Architecture targets are tracked
in [`docs/architecture`](docs/architecture/).

## Data plane shape

```
HTTP request
  │
  ▼
internal/middleware        ── request lifecycle (M1-M10): auth / envelope / budget /
                         catalog / ratelimit / moderation / **M7 → dispatch** /
                         tracing. Each middleware has a single responsibility,
                         wired via an OTel option.
  │
  ▼  (M7 is a thin adapter over dispatch.Dispatcher)
internal/dispatch          ── the **sole** owner of scheduling/execution sequencing:
                         CandidateSource → filterEligible → Selector.Pick →
                         EndpointQuota.Reserve → Handler lookup → Invoker →
                         Selector.Report → RetryPolicy → Stream / Charge
                         (internal/dispatch/adapters/ bridges selector / invoker /
                         ratelimit / repo primitives into dispatch ports)
  │
  ├── internal/selector    ── primitives: filters / scorer / picker / cooldown.
  │                      Pure selection algorithms, unaware of protocol / handler / middleware
  ├── internal/invoker     ── primitives: a single HTTP call + response forwarding, no scheduling
  ├── internal/ratelimit   ── primitives: Store / Bucket / endpoint bucket helpers
  └── internal/protocol    ── facade: Handler = Factory + Translator + Quirks
                         Consumers only see Handler / Lookup; Factory / Session are internal
       ├── protocol/<vendor>/  OpenAI(+ark alias) / Anthropic / Gemini Factory + Session
       ├── protocol/quirks/    endpoint-level body+header tweak DSL (rename/strip/set/set_default)
       └── translator (internal/)   body shape conversion: identity + cross-protocol pairs (assembled in internal/builtin)

internal/moderation        ── Moderator + response-stream decorator + ctx helpers
internal/usage             ── usage extraction + outbox (file | kafka); pricing is downstream
internal/trace / metric    ── Tracer abstraction (slog / OTel) + Prometheus metric name constants

internal/repo              ── data access: sqlx Reader/Provider + TTL LRU cache wrapper
                         (5 cached wrappers: APIKey / ModelService / Endpoint /
                         QuotaPolicy / Subscription; llm_gateway_repo_cache_total
                         metric reports hit/miss)
internal/infra             ── DB / Redis / Kafka adapters + schema.sql + Migrate
internal/domain            ── typed structs shared across packages (RequestContext / Endpoint / ...)
internal/config            ── gateway.yaml loader

internal/app/gateway  ── composition root: assembles data-plane dependencies
internal/builtin      ── the single built-in vendor/translator registration entry
cmd/gateway           ── thin data-plane process entry
cmd/console           ── optional control-plane Admin API
cmd/migrate           ── versioned database migration command
cmd/mockupstream      ── dev/test fake upstream
scripts/{e2e-smoke,seed-e2e}                       single-vendor end-to-end smoke test
scripts/{e2e-smoke-multivendor,seed-multivendor}   multi-vendor (openai/anthropic/gemini/cohere) end-to-end smoke test
docs/architecture/    design docs (00-overview through 08-observability)
configs/              per-environment config (local / prod / docker)
```

## Quick start

Run `make run-migrate` before the gateway in production. The bundled local and
Docker configs enable `database.auto_migrate` for development convenience.
Business data (model_services / endpoints / api_keys / pricing / quota_policies /
subscriptions / accounts) can be managed by SQL or the optional `cmd/console`
control-plane API; the data plane does not depend on the console.

```sh
# 1. Start the local stack (MySQL + Redis + Redpanda) via Docker.
make stack
# (or: docker compose up -d)

# 2. Migrate and start gateway (local config also auto-migrates idempotently).
make run-migrate
make run-gateway
# (or: go run ./cmd/gateway -config ./configs/local/gateway.yaml)

# 3. Insert a model_service + endpoint + api_key directly via SQL.
#    Example seed: see examples/full-config/seed.sql
mysql -h 127.0.0.1 -uroot llm_gateway < examples/full-config/seed.sql

# 4. Send a request.
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-test-alice" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hi!"}]}'
```

### Tests

```sh
make test               # unit tests; SQL tests skip without MYSQL_DSN
make test-integration   # bring up stack, run all tests including SQL/outbox
```

`gateway.yaml` controls server settings (addr, timeouts, body limit), the
database connection, outbox driver, and middleware tunables. Defaults are
sensible — see [`internal/config/config.go`](internal/config/config.go) for the full
schema.

The gateway listens on `:8080` by default. With the bundled config:

| Endpoint | Method | Notes |
|----------|--------|-------|
| `/healthz` | GET | liveness probe |
| `/readyz` | GET | readiness probe |
| `/metrics` | GET | Prometheus scrape |
| `/v1/chat/completions` | POST | OpenAI Chat Completions |
| `/v1/messages` | POST | Anthropic-style chat |
| `/v1/embeddings` | POST | OpenAI Embeddings |
| `/v1/images/{generations,edits,variations}` | POST | OpenAI Images |
| `/v1/audio/{speech,transcriptions,translations}` | POST | TTS + ASR |

Routes are defined per-modality under [`internal/router/`](internal/router/) — each
modality file (`chat.go` / `image.go` / `audio.go` / `embedding.go`) registers
its own paths and explicitly lists its middleware chain.

### Configuration files

Per-environment configs live under [`configs/`](configs/) (see
[`configs/README.md`](configs/README.md)).

A single environment directory contains one file:
- `gateway.yaml` — server / middleware / database / redis / outbox

Business data lives in MySQL. `cmd/migrate` applies versioned schema changes;
CRUD can use SQL directly or `cmd/console`. The repo layer
caches reads in-process with a TTL LRU (default ~30s), so updates become
visible within the TTL window. API-key revocation additionally supports
best-effort cachebus invalidation for sub-second propagation.

Reload of `gateway.yaml` requires restart.

## Build / test

```sh
go build ./...
go test ./...
```

## License

Apache-2.0 — see [LICENSE](LICENSE).
