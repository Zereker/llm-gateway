# ai-gateway Helm chart

把 ai-gateway 部署到 Kubernetes 的参考 chart。包括：

- gateway 多副本 + HPA
- admin 单副本（只在内网走，不暴露 ingress）
- ConfigMap 注入 yaml 配置
- Secret 持有 DSN / data_key / admin token / moderation API key
- envsubst 启动时把 env var 替换进 yaml（避免明文配置文件）

**前置依赖**（chart 不内置）：MySQL 8.0+ / Redis 7+ / Kafka 3+（生产推荐 cloud managed）。

## 镜像约定

需要构造一个含 `ai-gateway` + `ai-gateway-admin` 双 binary + `envsubst`（gettext 包）的镜像。
建议 Dockerfile：

```dockerfile
FROM golang:1.22 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/ai-gateway       ./cmd/gateway
RUN CGO_ENABLED=0 go build -o /out/ai-gateway-admin ./cmd/admin

FROM gcr.io/distroless/base-debian12:nonroot
# 注：distroless 没 envsubst；如果走 envsubst 模式要用 alpine 或单独 sidecar
# 也可以考虑改 chart 让 gateway 直接读 env var（要求改代码 config loader）
COPY --from=builder /out/ai-gateway       /usr/local/bin/
COPY --from=builder /out/ai-gateway-admin /usr/local/bin/
USER nonroot
```

或更简单的 alpine 方案：

```dockerfile
FROM golang:1.22 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/ai-gateway       ./cmd/gateway
RUN CGO_ENABLED=0 go build -o /out/ai-gateway-admin ./cmd/admin

FROM alpine:3.20
RUN apk add --no-cache gettext ca-certificates
COPY --from=builder /out/ai-gateway       /usr/local/bin/
COPY --from=builder /out/ai-gateway-admin /usr/local/bin/
USER 65532:65532
```

## 安装

```bash
# 1. 准备 secret 值（**不要 commit**）
cat > my-values.yaml <<EOF
secrets:
  databaseDSN: "user:pwd@tcp(mysql:3306)/ai_gateway?parseTime=true&charset=utf8mb4"
  redisAddr:   "redis:6379"
  redisPassword: "redis-pwd"
  dataKey:     "$(openssl rand -hex 32)"
  adminToken:  "$(openssl rand -hex 16)"
EOF

# 2. install
helm install ai-gw ./examples/k8s/ai-gateway -f my-values.yaml

# 3. 检查
kubectl get pods -l app.kubernetes.io/name=ai-gateway
kubectl logs -l app.kubernetes.io/component=gateway --tail=50
```

## 升级 / 回滚

```bash
helm upgrade ai-gw ./examples/k8s/ai-gateway -f my-values.yaml
helm rollback ai-gw 1
```

ConfigMap / Secret 改动会触发 deployment rolling restart（`checksum/config` annotation）。

## 卸载

```bash
helm uninstall ai-gw
# Note: secret 不会被自动删除（防止误删）
kubectl delete secret ai-gw-ai-gateway-secrets
```

## 生产建议

| 维度 | 建议 |
|---|---|
| 镜像 tag | 用版本号（如 `1.0.0`）；不要 `latest` |
| Secret 管理 | 用 ExternalSecrets / Sealed Secrets / vault sidecar；不要明文 values.yaml |
| Resource limits | gateway 流式占 goroutine 多于 CPU；按 QPS 起步给 cpu=2 / mem=2Gi 再调 |
| HPA 指标 | CPU 是次优；上 custom metric（in-flight requests / queue depth）效果更好 |
| Ingress | 走 nginx ingress + cert-manager 自动 TLS；body limit 配 10MiB+ |
| Network policy | gateway 只允许 ingress; admin 只允许同 namespace; deny everything else |

## 不在本 chart 范围

| 项 | 推荐方案 |
|---|---|
| MySQL | cloud RDS / bitnami/mysql chart |
| Redis | cloud ElastiCache / bitnami/redis chart |
| Kafka | cloud MSK / strimzi / bitnami/kafka chart |
| OTel collector | opentelemetry-collector chart |
| Prometheus / Grafana | kube-prometheus-stack |
