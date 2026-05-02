# ai-gateway

A Go-based gateway that routes LLM API requests to multiple upstream providers
(OpenAI, Anthropic, Google, AWS Bedrock, vLLM / Ollama self-hosted, etc.) under
one OpenAI-compatible interface.

## Status

**v0.1 MVP.** Interfaces are settling but not yet API-stable. See
[`docs/architecture/07-roadmap.md`](docs/architecture/07-roadmap.md) for the
roadmap to v1.0.

## Layout

```
ai-gateway/
├── cmd/
│   ├── gateway/         data plane: server lifecycle + e2e tests
│   └── admin/           control plane: CRUD APIs (placeholder, v0.5+)
├── pkg/
│   ├── config/          gateway.yaml loader (boot config)
│   ├── domain/          shared domain types (RequestContext, Endpoint, ...)
│   ├── infra/           infrastructure adapters (sqlx Open + schema; future kafka/redis/...)
│   ├── repo/            data-access layer: Reader/Writer interfaces + sqlx impls
│   ├── middleware/      M1-M10 + helpers + default impls
│   ├── router/          gin engine + per-modality route registration
│   │                      (chat / image / audio / embedding files)
│   ├── adapter/         vendor-pluggable adapters; Factory + Session contracts
│   │   └── openai/      OpenAI / OpenAI-compatible adapter
│   ├── schedule/        endpoint selection abstractions (v0.5+ full impl)
│   ├── ratelimit/       rate-limit checker abstractions (v0.5+ full impl)
│   ├── usage/           Usage extraction + outbox + pricing
│   ├── trace/           Tracer abstraction + SlogTracer default
│   └── metric/          Prometheus metric name constants
├── docs/architecture/   design docs (00-overview through 07-roadmap)
└── configs/            per-environment configurations (local / prod / ...)
```

## Quick start

```sh
# 1. Start the gateway (creates configs/local/gateway.db on first run).
go run ./cmd/gateway -config ./configs/local/gateway.yaml

# 2. Use cmd/admin (Stage 4) to insert a model_service + endpoint, OR seed
#    the DB directly with sqlite3:
sqlite3 configs/local/gateway.db <<SQL
INSERT INTO model_services (service_id, model, group_name)
  VALUES ('openai/gpt-4o', 'gpt-4o', 'default');
INSERT INTO endpoints (id, vendor, url, api_key, model, group_name)
  VALUES ('openai_main', 'openai', 'https://api.openai.com/v1/chat/completions',
          'sk-REPLACE-ME', 'gpt-4o', 'default');
SQL
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
forwards to the OpenAI endpoint stored in `configs/local/gateway.db`, and writes
a usage event to `/tmp/ai-gateway-usage.log`.

### Configuration files

Per-environment configs live under [`configs/`](configs/) (see
[`configs/README.md`](configs/README.md) for layout + secret-management
recommendations).

A single environment directory contains:
- `gateway.yaml` — server / middleware / paths / database
- `apikeys.json` — `{apiKeyString: UserIdentity}` map (still file-based)
- `gateway.db` — sqlite database holding `model_services` + `endpoints`
  (auto-created on first boot via `pkg/infra.Migrate`)

`paths.apikeys` and sqlite `database.dsn` are resolved relative to the yaml
file's location, so the directory is portable.

Reload requires restart in v0.1; hot-reload (gateway polling DB / pg LISTEN)
is in v0.5+.

## Build / test

```sh
go build ./...
go test ./...
```

## License

Apache-2.0 — see [LICENSE](LICENSE).
