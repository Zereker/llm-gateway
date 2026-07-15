ARG GO_VERSION=1.25.12
ARG BASE_IMAGE_REGISTRY=docker.io/library

FROM ${BASE_IMAGE_REGISTRY}/golang:${GO_VERSION}-alpine AS builder
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
    go build -trimpath -ldflags="-s -w" -o /out/runner ./examples/benchmark/runner && \
    go build -trimpath -ldflags="-s -w" -o /out/compare ./examples/benchmark/compare

FROM ${BASE_IMAGE_REGISTRY}/alpine:3.20 AS seed
COPY --from=builder /out/seed /seed
ENTRYPOINT ["/seed"]

FROM ${BASE_IMAGE_REGISTRY}/alpine:3.20 AS upstream
COPY --from=builder /out/upstream /upstream
EXPOSE 9092
ENTRYPOINT ["/upstream"]

FROM ${BASE_IMAGE_REGISTRY}/alpine:3.20 AS runner
COPY --from=builder /out/runner /runner
ENTRYPOINT ["/runner"]

FROM ${BASE_IMAGE_REGISTRY}/alpine:3.20 AS compare
COPY --from=builder /out/compare /compare
ENTRYPOINT ["/compare"]
