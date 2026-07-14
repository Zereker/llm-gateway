ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS builder
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal ./internal
COPY examples/support/seed-demo ./examples/support/seed-demo
COPY examples/benchmark ./examples/benchmark
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/seed ./examples/support/seed-demo && \
    go build -trimpath -ldflags="-s -w" -o /out/upstream ./examples/benchmark/upstream && \
    go build -trimpath -ldflags="-s -w" -o /out/runner ./examples/benchmark/runner

FROM alpine:3.20 AS seed
COPY --from=builder /out/seed /seed
ENTRYPOINT ["/seed"]

FROM alpine:3.20 AS upstream
COPY --from=builder /out/upstream /upstream
EXPOSE 9092
ENTRYPOINT ["/upstream"]

FROM alpine:3.20 AS runner
COPY --from=builder /out/runner /runner
ENTRYPOINT ["/runner"]
