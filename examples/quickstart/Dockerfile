# Quickstart-only images. The build context is the repository root.
#
# Usage (from examples/quickstart):
#   build:
#     context: ../..
#     dockerfile: examples/quickstart/Dockerfile
#
# Build args:
#   GO_VERSION  Go toolchain version (defaults to match go.mod)

ARG GO_VERSION=1.25
ARG BASE_IMAGE_REGISTRY=docker.io/library

# =============================================================================
# builder
# =============================================================================
FROM ${BASE_IMAGE_REGISTRY}/golang:${GO_VERSION}-alpine AS builder

ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src

# Copy go.mod / go.sum first so the download layer caches independently.
# Also works in sandboxed / offline environments via GOFLAGS=-mod=vendor plus the repo's own vendor/;
# by default this relies on proxy.golang.org (the CI default behavior).
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY examples/support/mockupstream ./examples/support/mockupstream
COPY examples/support/seed-demo ./examples/support/seed-demo

# CGO off -> fully static binary; -trimpath strips host paths; -s -w strips the symbol table to shrink size
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/console      ./cmd/console && \
    go build -trimpath -ldflags="-s -w" -o /out/gateway      ./cmd/gateway && \
    go build -trimpath -ldflags="-s -w" -o /out/mockupstream ./examples/support/mockupstream && \
    go build -trimpath -ldflags="-s -w" -o /out/seed-demo    ./examples/support/seed-demo

# =============================================================================
# console
# =============================================================================
FROM ${BASE_IMAGE_REGISTRY}/alpine:3.20 AS console
WORKDIR /app
COPY --from=builder /out/console /app/console
EXPOSE 8081
ENTRYPOINT ["/app/console"]
CMD ["-config", "/etc/llm-gateway/console.yaml"]

# =============================================================================
# gateway
# =============================================================================
FROM ${BASE_IMAGE_REGISTRY}/alpine:3.20 AS gateway
WORKDIR /app
COPY --from=builder /out/gateway /app/gateway
EXPOSE 8080
ENTRYPOINT ["/app/gateway"]
CMD ["-config", "/etc/llm-gateway/gateway.yaml"]

# =============================================================================
# mockupstream
# =============================================================================
FROM ${BASE_IMAGE_REGISTRY}/alpine:3.20 AS mockupstream
WORKDIR /app
COPY --from=builder /out/mockupstream /app/mockupstream
COPY internal/cassette/testdata/fieldmatrix /app/testdata/fieldmatrix
ENV LLM_GATEWAY_TESTDATA_DIR=/app/testdata
EXPOSE 9090
ENTRYPOINT ["/app/mockupstream"]

# =============================================================================
# seed (one-shot and idempotent)
# =============================================================================
FROM ${BASE_IMAGE_REGISTRY}/alpine:3.20 AS seed
WORKDIR /app
COPY --from=builder /out/seed-demo /app/seed-demo
ENTRYPOINT ["/app/seed-demo"]
