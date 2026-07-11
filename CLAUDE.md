# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

llm-gateway is a Go-implemented LLM inference gateway: it exposes OpenAI / Anthropic compatible protocols externally, and routes downstream to multiple upstreams (OpenAI, Anthropic, Gemini, vLLM, etc.). The single source of truth for architecture and contracts is `docs/architecture/00-overview.md` ~ `08-observability.md` — **read the corresponding chapter before changing main-path code**.

## Processes

The repo has two binaries: `cmd/gateway` (the data plane, :8080) and `cmd/console` (a separate
control-plane binary — an Admin API for managing business data, backed by `pkg/console`):

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
go test -run TestAuth ./pkg/middleware
MYSQL_DSN='root:@tcp(localhost:3306)/llm_gateway?parseTime=true&charset=utf8mb4' go test ./pkg/repo
```

`go test ./...` is the source of truth for CI; Make is just local convenience.

## Key Architecture Concepts

### Middleware Chain (M1-M10)

The request pipeline consists of 10 middlewares, **the order is explicitly listed in per-modality files such as `pkg/router/chat.go`** — do not extract a shared helper. Current order:

```
M1 TraceContext → M10 Tracing → M9 Recover → M2 Auth   (pre-Envelope, attached on the group)
→ WithSourceProtocol (path tagging) → M3 Envelope
→ M4 Budget → M5 ModelService → M8 Moderation → M6 Limit → Cache → M7 Schedule
```

The `Cache` stage (response cache; chat + embedding modalities only, a no-op when `cache.enabled=false`)
sits between M6 Limit and M7 Schedule: a hit returns directly, skipping the upstream. Embedding routes
mount a dedicated exact-match `EmbeddingCache` stage in the same slot.

M10 is registered **outside** Recover, but its finishing logic runs post-`c.Next()` (the onion's return leg) —
so in execution order it still "finishes last," but no abort (401/429/503) or recovered panic can escape
its metric / usage / audit logging (older versions attached at the end of the chain would get skipped on abort).

Each modality file (`chat.go` / `image.go` / `audio.go` / `embedding.go`) **lists its own** complete chain. Divergence is expected to grow (e.g. chat adds a Moderator, image adds a multipart Parser), so DRY is deliberately rejected here.

### RequestContext (P2)

Request-level state shared across middlewares goes through the `*domain.RequestContext` typed struct, passed via `gin.Context.Set/Get`. Fetch it with `middleware.GetRequestContext(c)`; scattered `c.Set("foo", ...)` calls are **forbidden**.

### Protocol facade (P3 / P4)

- End-to-end protocol handling goes through the `pkg/protocol.Handler` facade; consumers (dispatch / middleware / invoker) only see the `Handler` / `Lookup` interfaces, and **never touch** `Factory` / `Session` / `LookupFactory` directly.
- Internally, Handler = `Combine(Factory, translator.Translator)` + an endpoint-level `quirks.Rewriter`:
    - `pkg/protocol/<vendor>/`: the vendor HTTP layer (URL / auth header / Content-Type) — Factory + Session implementation; `init()` calls `protocol.RegisterFactory("<vendor>", Factory{})`.
    - `pkg/translator/<src>_<dst>/`: protocol shape conversion (OpenAI ↔ Anthropic / OpenAI ↔ Gemini / identity, etc.), `init()` calls `translator.Register(...)`.
    - `pkg/protocol/quirks`: an endpoint-level body + header tweak DSL (stored in the `endpoints.quirks` JSON column); driven by deployer config, not registered in code.
- Adding a new vendor / translator: register inside the sub-package's `init()` and add its single blank import to `internal/builtin/builtin.go`. Gateway and console both consume that built-in set.
- As of v0.7, `pkg/adapter` has been merged into `pkg/protocol`; references to `pkg/adapter/<vendor>/` in older docs are historical paths — the code now lives under `pkg/protocol/<vendor>/`.

### Client Protocol Scope

The gateway only exposes three **client**-facing entry points: OpenAI / Anthropic / OpenAI Responses. Gemini is only supported as an **upstream** — clients call via the OpenAI SDK, and the gateway translates to Gemini.

### Pluggable infra (P5)

All external dependencies go through interfaces: `BudgetGate` / `Moderator` / `Tracer` / `OutboxPublisher` / `ratelimit.Store` / `schedule.CooldownManager` / `repo.*Provider`. Dependency assembly lives in `internal/app/gateway`; config rejects unknown drivers before assembly.

### Error Classification (P7)

`domain.ErrTransient / ErrRateLimit / ErrPermanent / ErrInvalid / ErrUnknown`. Retry strategy + cooldown duration are determined by class; any new error handling must be attached to one of these five classes.

## Code Conventions

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
