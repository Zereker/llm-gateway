# ai-gateway

A unified entry point for routing requests to multiple LLM providers — both open-source and proprietary.

## Status

Early development. Interfaces and structure are not yet stable.

## Layout

This is a Go monorepo with two services:

- `cmd/gateway/` — **data plane**: receives client requests and routes them to the configured provider backend.
- `cmd/admin/` — **control plane**: configuration, observability, and key management for the gateway.

Shared, reusable code lives under `pkg/`. Service-private code lives under `internal/`.

## Build

```sh
go build ./...
go run ./cmd/gateway
go run ./cmd/admin
```

## License

Apache-2.0 — see [LICENSE](LICENSE).
