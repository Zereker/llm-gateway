# llm-gateway

[English](README.md) | [简体中文](README.zh-CN.md)

[![codecov](https://codecov.io/gh/Zereker/llm-gateway/graph/badge.svg)](https://codecov.io/gh/Zereker/llm-gateway)

**A policy-aware, OpenAI-compatible runtime gateway for enterprise LLM traffic.**

`llm-gateway` gives platform teams one enforcement point for authentication,
routing, quota, moderation, metering, audit, and observability across hosted and
self-hosted models. It is designed as a control plane plus data plane, not as a
thin reverse proxy.

[Quick start](#quick-start) · [Documentation](docs/) · [Architecture](docs/architecture/) ·
[Roadmap](docs/ROADMAP.md) · [Examples](examples/) · [Benchmark](examples/benchmark/) ·
[中文说明](README.zh-CN.md)

## Why llm-gateway

- **One client contract:** OpenAI, Anthropic, and other request shapes share one
  routing pipeline and can be translated to a different upstream protocol.
- **Reliable model traffic:** capability filtering, quota reservation, P2C,
  cooldown, success/latency scoring, retry, and explicit cross-model fallback.
- **Governed access:** account subscriptions, API-key and account quotas,
  input/output moderation hooks, content logging, and usage events.
- **Separated control plane:** the optional Console manages models, endpoints,
  keys, policies, pricing, and audit without becoming a data-plane dependency.
- **Operational by default:** Prometheus metrics, structured tracing, readiness,
  encrypted upstream credentials, and pluggable SQL/Redis/Kafka infrastructure.

```text
Applications / SDKs
        │  OpenAI / Anthropic-compatible APIs
        ▼
┌──────────────────── llm-gateway data plane ────────────────────┐
│ Auth → Catalog → Quota → Moderation → Dispatch → Usage / Trace │
│                                      │                         │
│                         filter → score → retry/fallback        │
└──────────────────────────────────────┼─────────────────────────┘
                                       ▼
                  OpenAI · Anthropic · Gemini · Bedrock
                  OpenAI-compatible APIs · vLLM · Ollama

        Console control plane ── MySQL ── data plane cache
```

## Current capabilities

| Area | Included today |
|---|---|
| APIs | OpenAI Chat/Responses, Anthropic Messages, embeddings |
| Upstreams | OpenAI-compatible providers, Anthropic, Gemini/Vertex, Bedrock, Cohere, Azure OpenAI |
| Routing | weighted/P2C selection, inflight awareness, cooldown, runtime scoring, retry, explicit model fallback |
| Governance | API-key auth, account subscriptions, layered quota, moderation chain, content log, write audit |
| Operations | Admin API + Web Console, Prometheus, OTel/slog tracing, usage-event publishing, Helm example |

Rule-based virtual models, scoped prompt policies, and enterprise identity are
roadmap items; they are not presented as completed features.

## Quick start

Run the self-contained quickstart with Docker. It starts MySQL, Redis, the gateway,
the Web Console, a mock LLM upstream, migration, and idempotent seed data:

```sh
make -C examples/quickstart up
```

The command verifies a real request through the gateway and prints the response.
After it finishes:

- Gateway: `http://localhost:8080`
- Console: `http://localhost:8081` using token `quickstart-admin-token`
- Demo API key: `sk-quickstart-llm-gateway`

```sh
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-quickstart-llm-gateway" \
  -H "Content-Type: application/json" \
  -d '{"model":"fast-chat","messages":[{"role":"user","content":"Hi!"}]}'

make -C examples/quickstart down
```

`fast-chat` is an account-scoped virtual model that the seeded routing policy
resolves to the concrete mock model. The quickstart uses the bundled mock upstream and development-only credentials. It
does not call or require a real model provider. Its Compose file, Dockerfile,
configuration, and lifecycle commands are isolated in [`examples/quickstart`](examples/quickstart/).

## Status

The core data plane, control plane, and multi-vendor protocol paths are
implemented. Architecture contracts are tracked in
[`docs/architecture`](docs/architecture/); product evolution and acceptance
gates are tracked in the outcome-based [`roadmap`](docs/ROADMAP.md).

## Data plane internals

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
internal/infra             ── DB / Redis / Kafka adapters + immutable SQL migrations
internal/domain            ── typed structs shared across packages (RequestContext / Endpoint / ...)
internal/config            ── gateway.yaml loader

internal/app/gateway  ── composition root: assembles data-plane dependencies
internal/builtin      ── the single built-in vendor/translator registration entry
cmd/gateway           ── thin data-plane process entry
cmd/console           ── optional control-plane Admin API
examples/             ── quickstart, local stack, benchmark, configs, and support programs
deploy/               ── supported Helm, production config, and observability assets
docs/architecture/    ── design docs (00-overview through 08-observability)
```

## Manual local development

Use this workflow when developing Go processes on the host instead of using the
self-contained quickstart. Gateway startup applies versioned schema migrations
before accepting traffic.
Business data (model_services / endpoints / api_keys / pricing / quota_policies /
routing policies / enforcement policies and bindings / subscriptions / accounts) can be managed by SQL or the optional `cmd/console`
control-plane API; the data plane does not depend on the console.

```sh
# 1. Start the local stack (MySQL + Redis + Redpanda) via Docker.
make dev-up
# (or: docker compose -f examples/local/compose.yaml up -d)

# 2. Start gateway; schema migration is automatic and idempotent.
make run-gateway
# (or: go run ./cmd/gateway -config ./examples/local/configs/gateway.yaml)

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
make cover              # unit tests + a coverage profile (same MYSQL_DSN/REDIS_ADDR gating)
```

The badge above is the live, per-file coverage from CI (`go` job in
[`.github/workflows/ci.yml`](.github/workflows/ci.yml), uploaded to
[codecov.io/gh/Zereker/llm-gateway](https://codecov.io/gh/Zereker/llm-gateway)) —
that job runs with MySQL/Redis/Kafka up, so it also covers the SQL/Redis-backed
suites `make cover` skips locally by default. For a local number before
pushing, run `make cover` (unit tests only, no `MYSQL_DSN`/`REDIS_ADDR`,
scoped to the complete `internal/...` tree; executable code without tests
remains visible at 0%); `go tool cover -html=coverage.txt` renders a per-line
breakdown in the browser.

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

Routes are defined per modality under [`internal/router/`](internal/router/).
Image generation and audio routes are not advertised until their multipart,
protocol-adapter, and black-box compatibility paths are complete.

### Configuration files

Local host-process configs live in [`examples/local/configs`](examples/local/configs/). The supported
production template lives in [`deploy/configs`](deploy/configs/), while each
quickstart or benchmark owns its scenario-specific configuration.

Business data lives in MySQL. Gateway startup applies versioned schema changes;
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
