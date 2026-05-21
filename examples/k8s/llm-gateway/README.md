# llm-gateway Helm chart

把 llm-gateway 部署到 Kubernetes 的参考 chart。包括：

- gateway 多副本 + HPA
- ConfigMap 注入 yaml 配置
- Secret 持有 DSN / data_key / moderation API key
- envsubst 启动时把 env var 替换进 yaml（避免明文配置文件）

**前置依赖**（chart 不内置）：MySQL 8.0+ / Redis 7+ / Kafka 3+（生产推荐 cloud managed）。

## 镜像约定

需要构造一个含 `llm-gateway` binary + `envsubst`（gettext 包）的镜像。
建议 Dockerfile：

```dockerfile
FROM golang:1.22 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/llm-gateway ./cmd/gateway

FROM alpine:3.20
RUN apk add --no-cache gettext ca-certificates
COPY --from=builder /out/llm-gateway /usr/local/bin/
USER 65532:65532
```

或 distroless（无 envsubst；改 chart 让 gateway 直接读 env var）：

```dockerfile
FROM golang:1.22 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/llm-gateway ./cmd/gateway

FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=builder /out/llm-gateway /usr/local/bin/
USER nonroot
```

## 安装

```bash
# 1. 准备 secret 值（**不要 commit**）
cat > my-values.yaml <<EOF
secrets:
  databaseDSN: "user:pwd@tcp(mysql:3306)/llm_gateway?parseTime=true&charset=utf8mb4"
  redisAddr:   "redis:6379"
  redisPassword: "redis-pwd"
  dataKey:     "$(openssl rand -hex 32)"
EOF

# 2. install
helm install ai-gw ./examples/k8s/llm-gateway -f my-values.yaml

# 3. 检查
kubectl get pods -l app.kubernetes.io/name=llm-gateway
kubectl logs -l app.kubernetes.io/component=gateway --tail=50
```

## 业务数据管理

本 chart 不提供控制面——业务数据（accounts / endpoints / api_keys 等）由
deployer 直接 SQL 写入 MySQL。生产推荐做法：

- 把 SQL 文件放进独立的 GitOps 仓库（ArgoCD app / kubectl job）
- 用 K8s Job + initContainer 在 gateway 启动前跑 INSERT 脚本
- 或者业务团队走自家管理系统（CRM / 计费系统）直接写 DB

gateway 启动期会自跑 `infra.Migrate` 建表（`schema.sql` 全 `IF NOT EXISTS` 幂等），
多副本同时启动也安全。

## 升级 / 回滚

```bash
helm upgrade ai-gw ./examples/k8s/llm-gateway -f my-values.yaml
helm rollback ai-gw 1
```

ConfigMap / Secret 改动会触发 deployment rolling restart（`checksum/config` annotation）。

## 卸载

```bash
helm uninstall ai-gw
# Note: secret 不会被自动删除（防止误删）
kubectl delete secret ai-gw-llm-gateway-secrets
```

## 生产建议

| 维度 | 建议 |
|---|---|
| 镜像 tag | 用版本号（如 `1.0.0`）；不要 `latest` |
| Secret 管理 | 用 ExternalSecrets / Sealed Secrets / vault sidecar；不要明文 values.yaml |
| Resource limits | gateway 流式占 goroutine 多于 CPU；按 QPS 起步给 cpu=2 / mem=2Gi 再调 |
| HPA 指标 | CPU 是次优；上 custom metric（in-flight requests / queue depth）效果更好 |
| Ingress | 走 nginx ingress + cert-manager 自动 TLS；body limit 配 10MiB+ |
| Network policy | gateway 只允许 ingress; deny everything else |

## 不在本 chart 范围

| 项 | 推荐方案 |
|---|---|
| MySQL | cloud RDS / bitnami/mysql chart |
| Redis | cloud ElastiCache / bitnami/redis chart |
| Kafka | cloud MSK / strimzi / bitnami/kafka chart |
| OTel collector | opentelemetry-collector chart |
| Prometheus / Grafana | kube-prometheus-stack |
