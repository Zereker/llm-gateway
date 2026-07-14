# Production images. Development, quickstart, and benchmark images live with
# their owning scenarios rather than in this file.

ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS builder
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

FROM alpine:3.20 AS gateway
WORKDIR /app
COPY --from=builder /out/gateway /app/gateway
EXPOSE 8080
ENTRYPOINT ["/app/gateway"]
CMD ["-config", "/etc/llm-gateway/gateway.yaml"]

FROM alpine:3.20 AS console
WORKDIR /app
COPY --from=builder /out/console /app/console
EXPOSE 8081
ENTRYPOINT ["/app/console"]
CMD ["-config", "/etc/llm-gateway/console.yaml"]
