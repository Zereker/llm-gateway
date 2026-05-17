# llm-gateway

A Go-based gateway that routes LLM API requests to multiple upstream providers
(OpenAI, Anthropic, Google, AWS Bedrock, vLLM / Ollama self-hosted, etc.) under
one OpenAI-compatible interface.

## Status

**v0.1 MVP.** Interfaces are settling but not yet API-stable. Architecture
targets are tracked in [`docs/architecture`](docs/architecture/).

## Layout

```
llm-gateway/
├── cmd/
│   ├── gateway/         data plane: serves /v1/* LLM requests, reads DB
│   └── admin/           control plane: CRUD APIs over model_services / endpoints,
│                          owns schema (runs Migrate on boot)
├── pkg/
│   ├── config/          gateway.yaml loader (boot config)
│   ├── domain/          shared domain types (RequestContext, Endpoint, ...)
│   ├── infra/           infrastructure adapters (sqlx + schema, kafka producer)
│   ├── server/          process lifecycle: open infra, run http, close in LIFO order
│   ├── repo/            data-access layer: Reader/Writer interfaces + sqlx impls
│   ├── middleware/      M1-M10 + helpers + default impls
│   ├── router/          gin engine + per-modality route registration
│   │                      (chat / image / audio / embedding files)
│   ├── adapter/         vendor-pluggable adapters; Factory + Session contracts
│   │   └── openai/      OpenAI / OpenAI-compatible adapter
│   ├── schedule/        endpoint selection abstractions (v0.5+ full impl)
│   ├── ratelimit/       rate-limit checker abstractions (v0.5+ full impl)
│   ├── usage/           Usage extraction + outbox (file | kafka) + pricing
│   ├── trace/           Tracer abstraction + SlogTracer default
│   └── metric/          Prometheus metric name constants
├── docs/architecture/   design docs (00-overview through 06-pluggable-infra)
└── configs/            per-environment configurations (local / prod / ...)
```

## Quick start

The two services are independent binaries. **Boot order: stack → admin → gateway**.
Admin owns the DB schema (runs `Migrate` on boot); gateway only reads.

```sh
# 1. Start the local stack (MySQL + Redis + Redpanda) via Docker.
docker compose up -d
# (or: make stack)

# 2. Start admin — connects to MySQL + creates tables.
make run-admin
# (or: go run ./cmd/admin -config ./configs/local/admin.yaml)

# 3. Insert a model_service + an endpoint via the admin REST API.
TOKEN=local-dev-token

curl -X POST http://localhost:8081/admin/v1/modelservices \
  -H "X-Admin-Token: $TOKEN" -H "Content-Type: application/json" \
  -d '{"service_id":"openai/gpt-4o","model":"gpt-4o","group":"default"}'

curl -X POST http://localhost:8081/admin/v1/endpoints \
  -H "X-Admin-Token: $TOKEN" -H "Content-Type: application/json" \
  -d '{"id":"openai_main","vendor":"openai","model":"gpt-4o","group":"default",
       "url":"https://api.openai.com/v1/chat/completions",
       "api_key":"sk-REPLACE-ME"}'

# 4. Start gateway — Open + repo.CheckSchema; fails fast if step 2 was skipped.
make run-gateway
```

### Tests

```sh
make test               # unit tests; SQL tests skip without MYSQL_DSN
make test-integration   # bring up stack, run all tests including SQL/outbox
```

`gateway.yaml` controls server settings (addr, timeouts, body limit), the
apikeys file path, and the database connection. Defaults are sensible — see
[`pkg/config/config.go`](pkg/config/config.go) for the full schema.

The gateway listens on `:8080` by default. With the bundled config:

| Endpoint | Method | Notes |
|----------|--------|-------|
| `/healthz` | GET | liveness probe |
| `/readyz` | GET | readiness probe |
| `/metrics` | GET | Prometheus scrape (v0.1: stub) |
| `/v1/chat/completions` | POST | OpenAI Chat Completions |
| `/v1/messages` | POST | Anthropic-style chat (v0.5+) |
| `/v1/embeddings` | POST | OpenAI Embeddings |
| `/v1/images/{generations,edits,variations}` | POST | OpenAI Images (v0.5+ adapter) |
| `/v1/audio/{speech,transcriptions,translations}` | POST | TTS + ASR (v0.5+ adapter) |

Routes are defined per-modality under [`pkg/router/`](pkg/router/) — each
modality file (`chat.go` / `image.go` / `audio.go` / `embedding.go`) registers
its own paths and explicitly lists its middleware chain.

### Send a request

```sh
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-test-alice" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hi!"}]}'
```

The gateway authenticates `sk-test-alice` against `configs/local/apikeys.json`,
forwards to the OpenAI endpoint stored in MySQL (`llm_gateway.endpoints`), and
writes a usage event to `/tmp/llm-gateway-usage.log` (file outbox; switch to
Kafka via `usage_events.driver: kafka` in gateway.yaml).

### Configuration files

Per-environment configs live under [`configs/`](configs/) (see
[`configs/README.md`](configs/README.md) for layout + secret-management
recommendations).

A single environment directory contains:
- `gateway.yaml` — server / middleware / database / redis / outbox
- `admin.yaml` — admin server config (separate binary, port :8081)

`accounts`, `api_keys`, `model_services`, `subscriptions`, `endpoints`, and
`quota_policies` live in MySQL — `cmd/admin` owns the schema
(runs `infra.Migrate` on boot) and exposes CRUD over `/admin/v1/...`.

Reload requires restart in v0.1; hot-reload (gateway polling DB / pg LISTEN)
is in v0.5+.

## Build / test

```sh
go build ./...
go test ./...
```

## License

Apache-2.0 — see [LICENSE](LICENSE).
