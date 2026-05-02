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
│   ├── gateway/         data plane: HTTP server + middleware chain
│   └── admin/           control plane: CRUD APIs (placeholder, v0.5+)
├── pkg/
│   ├── domain/          shared domain types (RequestContext, Endpoint, ...)
│   ├── middleware/      M1-M10 + helpers + default impls
│   ├── adapter/         vendor-pluggable adapters; Factory + Session contracts
│   │   └── openai/      OpenAI / OpenAI-compatible adapter
│   ├── schedule/        endpoint selection abstractions (v0.5+ full impl)
│   ├── ratelimit/       rate-limit checker abstractions (v0.5+ full impl)
│   ├── usage/           Usage extraction + outbox + pricing
│   ├── store/           watchable KV abstraction + FileKV default
│   ├── trace/           Tracer abstraction + SlogTracer default
│   └── metric/          Prometheus metric name constants
├── docs/architecture/   design docs (00-overview through 07-roadmap)
└── examples/            zero-deps starter config (apikeys + kv/)
```

## Quick start

```sh
# 1. Edit examples/kv/endpoint/openai_main.json to put your real OpenAI key
#    in the APIKey field.
# 2. Run:
go run ./cmd/gateway -config ./examples
```

The gateway listens on `:8080` by default. With the bundled config:

| Endpoint | Method | Notes |
|----------|--------|-------|
| `/healthz` | GET | liveness probe |
| `/readyz` | GET | readiness probe |
| `/metrics` | GET | Prometheus scrape (v0.1: stub) |
| `/v1/chat/completions` | POST | OpenAI Chat Completions |

### Send a request

```sh
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-test-alice" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hi!"}]}'
```

The gateway authenticates `sk-test-alice` against `examples/apikeys.json`,
forwards to the OpenAI endpoint configured in
`examples/kv/endpoint/openai_main.json`, and writes a usage event to
`/tmp/ai-gateway-usage.log`.

### Configuration files

- `examples/apikeys.json` — `{apiKeyString: UserIdentity}` map for the default
  `APIKeyProvider`.
- `examples/kv/modelservice/<id>.json` — one file per `domain.ModelServiceSnapshot`.
- `examples/kv/endpoint/<id>.json` — one file per `domain.Endpoint`. Put the real
  upstream API key in `APIKey`.

Reload requires restart in v0.1; hot-reload via fsnotify / etcd is in v0.5+.

## Build / test

```sh
go build ./...
go test ./...
```

## License

Apache-2.0 — see [LICENSE](LICENSE).
