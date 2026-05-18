# 多阶段：单一 builder 出三个 binary（admin / gateway / mockupstream），
# 三个 final 镜像各装一个，alpine 基底（busybox 自带 wget 给 healthcheck 用）。
#
# 用法（docker-compose 里）：
#   build:
#     context: .
#     target: admin
#
# build 参数：
#   GO_VERSION  Go toolchain 版本（默认对齐 go.mod）

ARG GO_VERSION=1.25

# =============================================================================
# builder
# =============================================================================
FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# 先拷 go.mod / go.sum 让 download 层独立缓存。
# 沙箱 / 无网环境下走 GOFLAGS=-mod=vendor + 仓库自带 vendor/ 时也能跑；
# 这里默认依赖 proxy.golang.org（CI 默认行为）。
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY pkg ./pkg

# CGO 关 → 纯静态 binary；-trimpath 去宿主路径；-s -w 去 symbol table 缩小体积
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/admin        ./cmd/admin && \
    go build -trimpath -ldflags="-s -w" -o /out/gateway      ./cmd/gateway && \
    go build -trimpath -ldflags="-s -w" -o /out/mockupstream ./cmd/mockupstream

# =============================================================================
# binaries-host: 用法 docker build --target binaries-host --output type=local,dest=bin/
# 把 builder 阶段产出的 binary 复制到本地（CI 出 artifact 用，本仓 e2e 不用）
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
# === host-prebuilt path：bin/ 已是宿主 go build 的产物 → 直接拷到 alpine ===
# 沙箱 / 离线场景：先 `make build` 让 bin/{gateway,admin,mockupstream} 存在，
# 再用 --target admin-prebuilt 等装配镜像，跳过 in-container go mod download。
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
