# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

llm-gateway is a Go-implemented LLM inference gateway: it exposes OpenAI / Anthropic compatible protocols externally, and routes downstream to multiple upstreams (OpenAI, Anthropic, Gemini, vLLM, etc.). The single source of truth for architecture and contracts is `docs/architecture/00-overview.md` ~ `08-observability.md` — **read the corresponding chapter before changing main-path code**.

## Processes

The repo has two binaries: `cmd/gateway` (the data plane, :8080) and `cmd/console` (a separate
control-plane binary — an Admin API for managing business data, backed by `internal/console`):

- `cmd/gateway` at startup runs `infra.Migrate` to create tables, plus `repo.CheckSchema` as a defensive check
- `cmd/gateway` handles `/v1/*` traffic; the middleware chain is M1-M10
- Business data (model_services / endpoints / api_keys / pricing / quota_policies /
  subscriptions / accounts) is still **maintained via direct SQL inserts**; the optional `cmd/console`
  Admin API is an additional way to manage it (and can publish cachebus invalidation, which
  `cmd/gateway` subscribes to for fast revocation).
  The repo layer uses an in-process TTL LRU cache (default 30s), so SQL changes become visible within ≤ TTL.

Startup order: **docker stack → gateway**. `data_key` (the KEK used to encrypt the endpoints.auth column)
must match the encryption used when the SQL inserts were made.

## Common Commands

```sh
make stack              # start mysql + redis + redpanda containers
make stack-clean        # stop containers and remove data volumes (full reset)

make test               # unit tests; SQL tests are skipped when MYSQL_DSN is not set
make test-integration   # start the stack + run the full suite serially (-p 1), including SQL / outbox

make build              # compile gateway + mockupstream into ./bin
make run-gateway        # run gateway (auto-runs infra.Migrate at startup)
make run-mockupstream   # run the mock upstream (for debugging)

# single test case (by package / by name)
go test -run TestAuth ./internal/middleware
MYSQL_DSN='root:@tcp(localhost:3306)/llm_gateway?parseTime=true&charset=utf8mb4' go test ./internal/repo
```

`go test ./...` is the source of truth for CI; Make is just local convenience.

## Key Architecture Concepts

### Middleware Chain (M1-M10)

The request pipeline consists of 10 middlewares. **The shared security / quota / observability order lives in one place, `internal/router/pipeline.go`** (`llmRouteGroup` + `registerLLMRoute`); each modality file supplies only how it differs (path, source protocol, modality, and its Cache stage) via `routeSpec`. Current order:

```
M1 TraceContext → M10 Tracing → M9 Recover → M2 Auth   (pre-Envelope, attached on the group)
→ WithSourceProtocol (path tagging) → M3 Envelope
→ M4 Budget → M5 ModelService → M6 Limit → M8 Moderation → Cache → M7 Schedule
```

The `Cache` stage (response cache; chat + embedding modalities only, a no-op when `cache.enabled=false`)
sits between M8 Moderation and M7 Schedule: a hit returns directly, skipping the upstream. Embedding routes
mount a dedicated exact-match `EmbeddingCache` stage in the same slot.

M10 is registered **outside** Recover, but its finishing logic runs post-`c.Next()` (the onion's return leg) —
so in execution order it still "finishes last," but no abort (401/429/503) or recovered panic can escape
its metric / usage / audit logging (older versions attached at the end of the chain would get skipped on abort).

The per-modality files (`chat.go` / `image.go` / `audio.go` / `embedding.go`) only declare their `routeSpec`s and delegate the chain to `registerLLMRoute`. The chain is identical across modalities except for the Cache slot: chat uses the exact-match `deps.Cache`, embedding uses `deps.EmbeddingCache`, image/audio pass none. When a genuinely modality-specific stage is needed later (e.g. an image multipart parser), add it as a `routeSpec` field rather than re-inlining a whole chain.

### RequestContext (P2)

Request-level state shared across middlewares goes through the `*requeststate.State` typed struct (`internal/requeststate`), attached to `c.Request.Context()` via `context.WithValue` (not `gin.Context.Set/Get`). Fetch it with `middleware.GetRequestContext(c)`; scattered `c.Set("foo", ...)` calls and untyped extension maps are **forbidden**.

### Protocol facade (P3 / P4)

- End-to-end protocol handling goes through the `internal/protocol.Handler` facade; consumers (dispatch / middleware / invoker) only see the `Handler` / `Lookup` interfaces, and **never touch** `Factory` / `Session` directly.
- Internally, Handler = `Combine(Factory, translator.Translator)` + an endpoint-level `quirks.Rewriter`:
    - `internal/protocol/<vendor>/`: the vendor HTTP layer (URL / auth header / Content-Type) — Factory + Session implementation.
    - `internal/translator/<src>_<dst>/`: protocol shape conversion (OpenAI ↔ Anthropic / OpenAI ↔ Gemini / identity, etc.).
    - `internal/protocol/quirks`: an endpoint-level body + header tweak DSL (stored in the `endpoints.quirks` JSON column); driven by deployer config, not registered in code.
- Vendor factories and translators are assembled **explicitly** in `internal/builtin.NewLookup` (`protocol.NewLookup(factories, translator.NewRegistry(...))`) — no process-global registries / `init()` side effects. `DefaultLookup` composes the per-request Handler from that application-scoped set. Gateway and console both consume the same built-in lookup.
- Adding a new vendor / translator: implement the Factory / Translator in its sub-package, then add it to the factory map / translator list in `internal/builtin.NewLookup`.
- As of v0.7, the former `pkg/adapter` has been merged into the protocol package; references to `pkg/adapter/<vendor>/` in older docs are historical paths — the code now lives under `internal/protocol/<vendor>/`.

### Client Protocol Scope

The gateway only exposes three **client**-facing entry points: OpenAI / Anthropic / OpenAI Responses. Gemini is only supported as an **upstream** — clients call via the OpenAI SDK, and the gateway translates to Gemini.

### Pluggable infra (P5)

All external dependencies go through interfaces: `BudgetGate` / `Moderator` / `Tracer` / `OutboxPublisher` / `ratelimit.Store` / `schedule.CooldownManager` / `repo.*Provider`. Dependency assembly lives in `internal/app/gateway`; config rejects unknown drivers before assembly.

### Error Classification (P7)

`domain.ErrTransient / ErrRateLimit / ErrPermanent / ErrInvalid / ErrUnknown`. Retry strategy + cooldown duration are determined by class; any new error handling must be attached to one of these five classes.

## Code Conventions

- **Package layout**: this is an application, not a library — all Go packages live under `internal/` (enforced by the Go compiler against external import). There is no `pkg/` directory; do not reintroduce one. `cmd/*` holds only the binaries' main wiring.
- **Path prefixes**: each route declares its full `/v1/...` path in its own `.POST` call — **do not** use `engine.Group("/v1")`. Reading chat.go should show the full URL at a glance.
- **`X-Gateway-*` headers**: all gateway custom headers use this prefix, to distinguish them from vendor / client headers. Client-overridable parameters (timeout / max_attempts / fallback_models) may only be **stricter** than the cfg defaults; parsing failures silently fall back.
- **Config driver paths**: all pluggable implementations are selected via a `driver:` field in yaml (`alwayspass` / `inmemory` / `slog` / `otel` / `file` / `kafka` / `none` / `openai`, etc.); the `build*` functions in `cmd/*/main.go` switch on it to the concrete implementation.
- **Endpoint credential encryption**: the `endpoints.auth` column is encrypted with AES-256-GCM; the KEK comes from `cfg.DataKey` (hex-encoded 32 bytes); when the deployer inserts encrypted ciphertext via SQL it must use the same KEK.

## Docs & Requirements

- Architecture and interface contracts: `docs/architecture/00-overview.md` ~ `08-observability.md`; a PR that changes the main path must update the corresponding doc in the same PR.
- **Requirement docs** (technical design / usage docs / test docs / launch checklists) should, per the user's global convention, all be written to Obsidian at `~/Documents/Obisdian/notebook/需求池/{需求名}/` — **do not** create new requirement-type docs under the repo's `docs/` directory.

## Git

- Commit messages **do not** include a `Co-Authored-By` line (per the user's global convention).
- `git push --force` / `git reset --hard` / `git rebase` and other operations that rewrite remote history are strictly forbidden; fixing an already-pushed commit must be done via `git revert` + a new commit.
