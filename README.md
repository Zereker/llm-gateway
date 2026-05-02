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
│   ├── middleware/      M1-M10 + helpers + default impls
│   ├── router/          gin engine + per-modality route registration
│   │                      (chat / image / audio / embedding files)
│   ├── adapter/         vendor-pluggable adapters; Factory + Session contracts
│   │   └── openai/      OpenAI / OpenAI-compatible adapter
│   ├── schedule/        endpoint selection abstractions (v0.5+ full impl)
│   ├── ratelimit/       rate-limit checker abstractions (v0.5+ full impl)
│   ├── usage/           Usage extraction + outbox + pricing
│   ├── store/           watchable KV abstraction + FileKV default
│   ├── trace/           Tracer abstraction + SlogTracer default
│   └── metric/          Prometheus metric name constants
├── docs/architecture/   design docs (00-overview through 07-roadmap)
└── configs/            per-environment configurations (local / prod / ...)
```

## Quick start

```sh
# 1. Edit configs/kv/endpoint/openai_main.json to put your real OpenAI key
#    in the APIKey field.
# 2. Run:
go run ./cmd/gateway -config ./configs/local/gateway.yaml
```

`gateway.yaml` controls server settings (addr, timeouts, body limit) and the
paths to data files (apikeys.json, kv root, usage log). Defaults are sensible
— see [`pkg/config/config.go`](pkg/config/config.go) for the full schema.

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
its own paths and the middleware chain via `buildChain(deps)`.

### Send a request

```sh
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-test-alice" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hi!"}]}'
```

The gateway authenticates `sk-test-alice` against `configs/apikeys.json`,
forwards to the OpenAI endpoint configured in
`configs/kv/endpoint/openai_main.json`, and writes a usage event to
`/tmp/ai-gateway-usage.log`.

### Configuration files

Per-environment configs live under [`configs/`](configs/) (see
[`configs/README.md`](configs/README.md) for layout + secret-management
recommendations).

A single environment directory contains:
- `gateway.yaml` — server / middleware / paths
- `apikeys.json` — `{apiKeyString: UserIdentity}` map
- `kv/modelservice/<id>.json` — one file per `domain.ModelServiceSnapshot`
- `kv/endpoint/<id>.json` — one file per `domain.Endpoint` (contains real
  upstream API key)

Paths in `gateway.yaml` are resolved relative to the yaml file's location, so
the directory is portable.

Reload requires restart in v0.1; hot-reload via fsnotify / etcd is in v0.5+.

## Build / test

```sh
go build ./...
go test ./...
```

## License

Apache-2.0 — see [LICENSE](LICENSE).
