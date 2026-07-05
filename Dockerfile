# Multi-stage: a single builder produces three binaries (admin / gateway / mockupstream),
# each of the three final images installs one, on an alpine base (busybox ships wget, used by healthcheck).
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
COPY pkg ./pkg

# CGO off -> fully static binary; -trimpath strips host paths; -s -w strips the symbol table to shrink size
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/admin        ./cmd/admin && \
    go build -trimpath -ldflags="-s -w" -o /out/gateway      ./cmd/gateway && \
    go build -trimpath -ldflags="-s -w" -o /out/mockupstream ./cmd/mockupstream

# =============================================================================
# binaries-host: usage docker build --target binaries-host --output type=local,dest=bin/
# Copies the binaries produced by the builder stage to the local filesystem (used for CI artifacts, not used by this repo's e2e)
# =============================================================================
FROM scratch AS binaries-host
COPY --from=builder /out/admin        /admin
COPY --from=builder /out/gateway      /gateway
COPY --from=builder /out/mockupstream /mockupstream

# =============================================================================
# admin
# =============================================================================
FROM alpine:3.20 AS admin
WORKDIR /app
COPY --from=builder /out/admin /app/admin
EXPOSE 8081
ENTRYPOINT ["/app/admin"]
CMD ["-config", "/etc/llm-gateway/admin.yaml"]

# =============================================================================
# gateway
# =============================================================================
FROM alpine:3.20 AS gateway
WORKDIR /app
COPY --from=builder /out/gateway /app/gateway
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
# Sandboxed / offline scenario: run `make build` first so bin/{gateway,admin,mockupstream} exist,
# then assemble the image with --target admin-prebuilt etc., skipping the in-container go mod download.
# =============================================================================
FROM alpine:3.20 AS admin-prebuilt
WORKDIR /app
COPY bin/llm-gateway-admin /app/admin
EXPOSE 8081
ENTRYPOINT ["/app/admin"]
CMD ["-config", "/etc/llm-gateway/admin.yaml"]

FROM alpine:3.20 AS gateway-prebuilt
WORKDIR /app
COPY bin/llm-gateway /app/gateway
EXPOSE 8080
ENTRYPOINT ["/app/gateway"]
CMD ["-config", "/etc/llm-gateway/gateway.yaml"]

FROM alpine:3.20 AS mockupstream-prebuilt
WORKDIR /app
COPY bin/llm-gateway-mockup /app/mockupstream
EXPOSE 9090
ENTRYPOINT ["/app/mockupstream"]
