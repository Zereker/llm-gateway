# llm-gateway

A Go-based gateway that routes LLM API requests to multiple upstream providers
(OpenAI, Anthropic, Google, AWS Bedrock, vLLM / Ollama self-hosted, etc.) under
one OpenAI-compatible interface.

## Status

**v0.1 MVP.** Interfaces are settling but not yet API-stable. Architecture
targets are tracked in [`docs/architecture`](docs/architecture/).

## Data plane shape

```
HTTP request
  │
  ▼
pkg/middleware        ── 请求生命周期（M1-M10）：auth / envelope / budget /
                         catalog / ratelimit / moderation / **M7 → dispatch** /
                         tracing。每个 middleware 单一职责，配 OTel option。
  │
  ▼  (M7 是 dispatch.Dispatcher 的 thin adapter)
pkg/dispatch          ── 调度执行时序的**唯一**所有者：
                         CandidateSource → filterEligible → Selector.Pick →
                         EndpointQuota.Reserve → Handler lookup → Invoker →
                         Selector.Report → RetryPolicy → Stream / Charge
                         （pkg/dispatch/adapters/ 桥接 selector / invoker /
                         ratelimit / repo primitives → dispatch ports）
  │
  ├── pkg/selector    ── primitives：filters / scorer / picker / cooldown。
  │                      只做选择算法，不知道 protocol / handler / middleware
  ├── pkg/invoker     ── primitives：一次 HTTP 调用 + 响应 forward，不做调度
  ├── pkg/ratelimit   ── primitives：Store / Bucket / endpoint bucket helpers
  └── pkg/protocol    ── facade：Handler = Factory + Translator + Quirks
                         消费侧只看 Handler / Lookup；Factory / Session 是内部
       ├── protocol/<vendor>/  OpenAI(+ark alias) / Anthropic / Gemini Factory + Session
       ├── protocol/quirks/    endpoint 级 body+header 微调 DSL（rename/strip/set/set_default）
       └── translator (pkg/)   body shape 转换：identity + 跨协议 pairs（init() 注册）

pkg/moderation        ── Moderator + 响应流装饰器 + ctx helpers
pkg/usage             ── usage 提取 + outbox（file | kafka）+ pricing
pkg/trace / metric    ── Tracer 抽象（slog / OTel）+ Prom metric name 常量

pkg/repo              ── data access：sqlx Reader/Provider + TTL LRU cache wrapper
                         （5 个 cached wrapper：APIKey / ModelService / Endpoint /
                         QuotaPolicy / Subscription；llm_gateway_repo_cache_total
                         metric 上报 hit/miss）
pkg/infra             ── DB / Redis / Kafka adapters + schema.sql + Migrate
pkg/domain            ── 跨包共享 typed structs（RequestContext / Endpoint / ...）
pkg/config            ── gateway.yaml loader

cmd/gateway           ── composition root：buildEngine 装配所有 deps，无业务逻辑
cmd/mockupstream      ── dev/test 假上游
scripts/{e2e-smoke,seed-e2e}  端到端烟测
docs/architecture/    设计文档（00-overview 至 08-observability）
configs/              per-environment 配置（local / prod / docker）
```

## Quick start

Gateway is one binary that runs `infra.Migrate` on boot to bootstrap the schema.
Business data (model_services / endpoints / api_keys / pricing / quota_policies /
subscriptions / accounts) is managed by inserting SQL directly into MySQL —
this repository does not ship a control-plane / management REST API.

```sh
# 1. Start the local stack (MySQL + Redis + Redpanda) via Docker.
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
create tables; CRUD is performed by inserting SQL directly. The repo layer
caches reads in-process with a TTL LRU (default ~30s), so updates become
visible within the TTL window without an invalidation channel.

Reload of `gateway.yaml` requires restart.

## Build / test

```sh
go build ./...
go test ./...
```

## License

Apache-2.0 — see [LICENSE](LICENSE).
