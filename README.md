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
│   ├── gateway/           data plane: serves /v1/* LLM requests, reads DB,
│   │                      runs infra.Migrate on boot to bootstrap schema
│   └── mockupstream/      dev/test helper: fake upstream that returns canned
│                          usage; used for local smoke testing
├── pkg/
│   ├── config/            gateway.yaml loader (boot config)
│   ├── domain/            shared domain types (RequestContext, Endpoint, ...)
│   ├── infra/             infra adapters + schema.sql + Migrate
│   ├── server/            process lifecycle: open infra, run http, close in LIFO
│   ├── repo/              data-access layer: Reader interfaces + sqlx impls
│   ├── middleware/        M1-M10 + helpers + default impls
│   ├── router/            gin engine + per-modality route registration
│   │                      (chat / image / audio / embedding files)
│   ├── dispatch/          调度执行：Dispatcher + Policies +
│   │                      Selector / InvokerFactory / EndpointQuota /
│   │                      CandidateSource ports
│   │   └── adapters/      默认 port 实现（组合 selector / invoker / ratelimit primitives）
│   ├── selector/          primitives：filters / scorer / picker / cooldown / Scheduler
│   ├── invoker/           primitives：HTTP call + forward stream
│   ├── protocol/          facade：Handler = Combine(adapter, translator)
│   │   ├── openai/        OpenAI vendor + ark alias
│   │   ├── anthropic/     Anthropic vendor
│   │   └── gemini/        Google Gemini vendor
│   ├── adapter/           internal contract（被 protocol.Combine 包成 Handler）
│   ├── translator/        body shape 转换：identity + 跨协议 pairs
│   ├── moderation/        Moderator + 响应流装饰器 + ctx helpers
│   ├── ratelimit/         primitives：Store + Bucket + endpoint bucket helpers
│   ├── usage/             Usage extraction + outbox (file | kafka) + pricing
│   ├── trace/             Tracer abstraction + SlogTracer default
│   └── metric/            Prometheus metric name constants
├── docs/architecture/     design docs (00-overview through 08-observability)
└── configs/               per-environment configurations (local / prod / docker)
```

## Quick start

Gateway is one binary that runs `infra.Migrate` on boot to bootstrap the schema.
Business data (model_services / endpoints / api_keys / pricing / quota_policies /
subscriptions / accounts) is managed by inserting SQL directly into MySQL —
this repository does not ship a control-plane / admin REST API.

```sh
# 1. Start the local stack (MySQL + Redis + Redpanda + Debezium) via Docker.
make stack
# (or: docker compose up -d)

# 2. Start gateway — runs infra.Migrate on boot to create tables.
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
sensible — see [`pkg/config/config.go`](pkg/config/config.go) for the full
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

Routes are defined per-modality under [`pkg/router/`](pkg/router/) — each
modality file (`chat.go` / `image.go` / `audio.go` / `embedding.go`) registers
its own paths and explicitly lists its middleware chain.

### Configuration files

Per-environment configs live under [`configs/`](configs/) (see
[`configs/README.md`](configs/README.md)).

A single environment directory contains one file:
- `gateway.yaml` — server / middleware / database / redis / outbox

Business data lives in MySQL. The gateway runs `infra.Migrate` on boot to
create tables; CRUD is performed by inserting SQL directly. Debezium CDC pushes
binlog changes into Redis Streams, and the gateway invalidates its L1 cache in
real time.

Reload of `gateway.yaml` requires restart.

## Build / test

```sh
go build ./...
go test ./...
```

## License

Apache-2.0 — see [LICENSE](LICENSE).
