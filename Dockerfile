# Production images. Development, quickstart, and benchmark images live with
# their owning scenarios rather than in this file.

ARG GO_VERSION=1.25
ARG BASE_IMAGE_REGISTRY=docker.io/library

FROM ${BASE_IMAGE_REGISTRY}/golang:${GO_VERSION}-alpine AS builder
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway && \
    go build -trimpath -ldflags="-s -w" -o /out/console ./cmd/console

FROM ${BASE_IMAGE_REGISTRY}/alpine:3.20 AS gateway
WORKDIR /app
COPY --from=builder /out/gateway /app/gateway
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/app/gateway"]
CMD ["-config", "/etc/llm-gateway/gateway.yaml"]

FROM ${BASE_IMAGE_REGISTRY}/alpine:3.20 AS console
WORKDIR /app
COPY --from=builder /out/console /app/console
USER 65532:65532
EXPOSE 8081
ENTRYPOINT ["/app/console"]
CMD ["-config", "/etc/llm-gateway/console.yaml"]
