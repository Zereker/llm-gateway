# Demo-only images. The build context is the repository root.
#
# Usage (from examples/demo):
#   build:
#     context: ../..
#     dockerfile: examples/demo/Dockerfile
#
# Build args:
#   GO_VERSION  Go toolchain version (defaults to match go.mod)

ARG GO_VERSION=1.25

# =============================================================================
# builder
# =============================================================================
FROM golang:${GO_VERSION}-alpine AS builder

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
COPY scripts/seed-e2e ./scripts/seed-e2e

# CGO off -> fully static binary; -trimpath strips host paths; -s -w strips the symbol table to shrink size
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/console      ./cmd/console && \
    go build -trimpath -ldflags="-s -w" -o /out/gateway      ./cmd/gateway && \
    go build -trimpath -ldflags="-s -w" -o /out/mockupstream ./cmd/mockupstream && \
    go build -trimpath -ldflags="-s -w" -o /out/seed-e2e     ./scripts/seed-e2e

FROM builder AS benchmark-builder
COPY examples/benchmark ./examples/benchmark
RUN go build -trimpath -ldflags="-s -w" -o /out/benchmark-upstream ./examples/benchmark/upstream && \
    go build -trimpath -ldflags="-s -w" -o /out/benchmark-runner   ./examples/benchmark/runner

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
EXPOSE 8080
ENTRYPOINT ["/app/gateway"]
CMD ["-config", "/etc/llm-gateway/gateway.yaml"]

# =============================================================================
# mockupstream
# =============================================================================
FROM alpine:3.20 AS mockupstream
WORKDIR /app
COPY --from=builder /out/mockupstream /app/mockupstream
COPY testdata/fieldmatrix /app/testdata/fieldmatrix
ENV LLM_GATEWAY_TESTDATA_DIR=/app/testdata
EXPOSE 9090
ENTRYPOINT ["/app/mockupstream"]

# =============================================================================
# seed (one-shot and idempotent)
# =============================================================================
FROM alpine:3.20 AS seed
WORKDIR /app
COPY --from=builder /out/seed-e2e /app/seed-e2e
ENTRYPOINT ["/app/seed-e2e"]

FROM alpine:3.20 AS benchmark-upstream
WORKDIR /app
COPY --from=benchmark-builder /out/benchmark-upstream /app/upstream
EXPOSE 9092
ENTRYPOINT ["/app/upstream"]

FROM alpine:3.20 AS benchmark-runner
WORKDIR /app
COPY --from=benchmark-builder /out/benchmark-runner /app/runner
ENTRYPOINT ["/app/runner"]
