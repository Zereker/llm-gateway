# Multi-stage builder for gateway, console, migrate, and mockupstream.
#
# Usage (in docker-compose):
#   build:
#     context: .
#     target: admin
#
# Build args:
#   GO_VERSION  Go toolchain version (defaults to match go.mod)

ARG GO_VERSION=1.25

# =============================================================================
# builder
# =============================================================================
FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Copy go.mod / go.sum first so the download layer caches independently.
# Also works in sandboxed / offline environments via GOFLAGS=-mod=vendor plus the repo's own vendor/;
# by default this relies on proxy.golang.org (the CI default behavior).
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg

# CGO off -> fully static binary; -trimpath strips host paths; -s -w strips the symbol table to shrink size
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/console      ./cmd/console && \
	go build -trimpath -ldflags="-s -w" -o /out/migrate      ./cmd/migrate && \
    go build -trimpath -ldflags="-s -w" -o /out/gateway      ./cmd/gateway && \
    go build -trimpath -ldflags="-s -w" -o /out/mockupstream ./cmd/mockupstream

# =============================================================================
# binaries-host: usage docker build --target binaries-host --output type=local,dest=bin/
# Copies the binaries produced by the builder stage to the local filesystem (used for CI artifacts, not used by this repo's e2e)
# =============================================================================
FROM scratch AS binaries-host
COPY --from=builder /out/console      /console
COPY --from=builder /out/migrate      /migrate
COPY --from=builder /out/gateway      /gateway
COPY --from=builder /out/mockupstream /mockupstream

# =============================================================================
# console
# =============================================================================
FROM alpine:3.20 AS console
WORKDIR /app
COPY --from=builder /out/console /app/console
EXPOSE 8081
ENTRYPOINT ["/app/console"]
CMD ["-config", "/etc/llm-gateway/console.yaml"]

# =============================================================================
# gateway
# =============================================================================
FROM alpine:3.20 AS gateway
WORKDIR /app
COPY --from=builder /out/gateway /app/gateway
COPY --from=builder /out/migrate /app/migrate
EXPOSE 8080
ENTRYPOINT ["/app/gateway"]
CMD ["-config", "/etc/llm-gateway/gateway.yaml"]

# =============================================================================
# mockupstream
# =============================================================================
FROM alpine:3.20 AS mockupstream
WORKDIR /app
COPY --from=builder /out/mockupstream /app/mockupstream
EXPOSE 9090
ENTRYPOINT ["/app/mockupstream"]

# =============================================================================
# === host-prebuilt path: bin/ already holds the host's go build output -> copy straight into alpine ===
# Sandboxed / offline scenario: run `make build` first, then assemble a
# `console-prebuilt` / `gateway-prebuilt` / `mockupstream-prebuilt` target.
# =============================================================================
FROM alpine:3.20 AS console-prebuilt
WORKDIR /app
COPY bin/llm-gateway-console /app/console
EXPOSE 8081
ENTRYPOINT ["/app/console"]
CMD ["-config", "/etc/llm-gateway/console.yaml"]

FROM alpine:3.20 AS gateway-prebuilt
WORKDIR /app
COPY bin/llm-gateway /app/gateway
COPY bin/llm-gateway-migrate /app/migrate
EXPOSE 8080
ENTRYPOINT ["/app/gateway"]
CMD ["-config", "/etc/llm-gateway/gateway.yaml"]

FROM alpine:3.20 AS mockupstream-prebuilt
WORKDIR /app
COPY bin/llm-gateway-mockup /app/mockupstream
EXPOSE 9090
ENTRYPOINT ["/app/mockupstream"]
