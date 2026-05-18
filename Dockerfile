# 多阶段构建：一份 builder 出三个 binary（admin / gateway / mockupstream），
# 三个最终镜像各自只装一个 binary，保持单职责 + 小体积。
#
# 用法（docker-compose 里）：
#   build:
#     context: .
#     target: admin
#
# build 参数：
#   GO_VERSION  — Go toolchain 版本（默认对齐 go.mod）
#
# 体积参考（amd64 distroless）：
#   admin       ~25MB
#   gateway     ~30MB
#   mockupstream ~10MB

ARG GO_VERSION=1.25
FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# 先拷 go.mod / go.sum 让 vendor 层独立缓存
COPY go.mod go.sum ./
RUN go mod download

# 再拷源码
COPY cmd ./cmd
COPY pkg ./pkg

# CGO 关掉走纯 Go 二进制；-trimpath 让 stack trace 不带宿主路径
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/admin        ./cmd/admin && \
    go build -trimpath -ldflags="-s -w" -o /out/gateway      ./cmd/gateway && \
    go build -trimpath -ldflags="-s -w" -o /out/mockupstream ./cmd/mockupstream

# Schema 文件单独留到 admin 镜像里供启动期 Migrate
# （cmd/admin 用 embed？这里走 bind，admin 容器外挂 /etc/llm-gateway/schema.sql）
# 当前 admin 走 GORM AutoMigrate；不需要 schema.sql

# =============================================================================
# admin
# =============================================================================
FROM alpine:3.20 AS admin
RUN apk add --no-cache ca-certificates tzdata curl
WORKDIR /app
COPY --from=builder /out/admin /app/admin
EXPOSE 8081
ENTRYPOINT ["/app/admin"]
CMD ["-config", "/etc/llm-gateway/admin.yaml"]

# =============================================================================
# gateway
# =============================================================================
FROM alpine:3.20 AS gateway
RUN apk add --no-cache ca-certificates tzdata curl
WORKDIR /app
COPY --from=builder /out/gateway /app/gateway
EXPOSE 8080
ENTRYPOINT ["/app/gateway"]
CMD ["-config", "/etc/llm-gateway/gateway.yaml"]

# =============================================================================
# mockupstream
# =============================================================================
FROM alpine:3.20 AS mockupstream
RUN apk add --no-cache ca-certificates curl
WORKDIR /app
COPY --from=builder /out/mockupstream /app/mockupstream
EXPOSE 9090
ENTRYPOINT ["/app/mockupstream"]
