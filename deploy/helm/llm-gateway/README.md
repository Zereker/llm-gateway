# llm-gateway Helm chart

A reference chart for deploying llm-gateway to Kubernetes. Includes:

- gateway multi-replica + HPA
- ConfigMap injecting yaml configuration
- Secret holding DSN / data_key / moderation API key
- direct `LLM_GATEWAY_*` secret environment overrides

**Prerequisites** (not bundled with the chart): MySQL 8.0+ / Redis 7+ / Kafka 3+ (cloud managed recommended for production).

## Image convention

The repository Dockerfile's `gateway` target contains the production data-plane
binary. Schema migration is part of gateway startup:

```dockerfile
FROM golang:1.25.12 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gateway ./cmd/gateway

FROM alpine:3.20
COPY --from=builder /out/gateway /app/gateway
USER 65532:65532
```

## Installation

```bash
# 1. Create a Secret outside the chart (**do not commit real values**).
kubectl create secret generic llm-gateway-production-secrets \
  --from-literal=database-dsn='user:pwd@tcp(mysql:3306)/llm_gateway?parseTime=true&charset=utf8mb4' \
  --from-literal=redis-addr='redis:6379' \
  --from-literal=redis-password='redis-pwd' \
  --from-literal=data-key="$(openssl rand -hex 32)" \
  --from-literal=moderation-api-key=''

# 2. Reference it from non-secret values.
cat > my-values.yaml <<EOF
secrets:
  existingSecret: llm-gateway-production-secrets
image:
  tag: "v0.1.0"
EOF

# 3. Install.
helm install ai-gw ./deploy/helm/llm-gateway -f my-values.yaml

# 4. Check.
kubectl get pods -l app.kubernetes.io/name=llm-gateway
kubectl logs -l app.kubernetes.io/component=gateway --tail=50
```

For local evaluation, `secrets.databaseDSN` and `secrets.dataKey` can be
provided inline instead. They have no defaults and rendering fails when
neither inline values nor `secrets.existingSecret` is configured.

The chart defaults to synchronous Kafka usage-event publishing. Selecting
`file` also requires either
`gateway.usagePersistence.enabled=true` or an explicit
`gateway.usagePersistence.existingClaim`. Multi-replica file modes require
shared `ReadWriteMany` storage; an ephemeral `emptyDir` is never treated as
durable storage.

## Business data management

The chart deploys only the data plane. Business data can be written directly
through SQL or managed by a separately deployed `cmd/console` control plane.

- Put SQL files into a separate GitOps repo (ArgoCD app / kubectl job)
- Use a K8s Job + initContainer to run INSERT scripts before gateway starts
- Or have the business team write directly to the DB via their own management system (CRM / billing system)

Every gateway replica runs the idempotent, versioned migration routine before
becoming ready. Migration records use insert-if-absent semantics. The database
user therefore needs the DDL permissions required by the checked-in immutable
migrations. Migration and schema validation must finish within the gateway's
30-second startup deadline.

## Upgrade / Rollback

```bash
helm upgrade ai-gw ./deploy/helm/llm-gateway -f my-values.yaml
helm rollback ai-gw 1
```

ConfigMap / Secret changes trigger a deployment rolling restart (`checksum/config` annotation).

## Uninstall

```bash
helm uninstall ai-gw
# A Secret referenced through secrets.existingSecret is external to the release
# and is not deleted by Helm.
kubectl delete secret llm-gateway-production-secrets
```

## Production recommendations

| Dimension | Recommendation |
|---|---|
| Image tag | Use an immutable release tag (e.g. `v0.1.0`); do not use `latest` |
| Secret management | Use `secrets.existingSecret` with ExternalSecrets / Sealed Secrets / Vault; do not use plaintext values.yaml |
| Usage file storage | File-based drivers require persistent RWX storage for multi-replica deployments; the default is Kafka |
| Resource limits | gateway streaming consumes more goroutines than CPU; start with cpu=2 / mem=2Gi based on QPS and tune from there |
| HPA metrics | CPU is suboptimal; custom metrics (in-flight requests / queue depth) work better |
| Ingress | Use nginx ingress + cert-manager for automatic TLS; set body limit to 10MiB+ |
| Network policy | gateway allows ingress only; deny everything else |

## Out of scope for this chart

| Item | Recommended solution |
|---|---|
| MySQL | cloud RDS / bitnami/mysql chart |
| Redis | cloud ElastiCache / bitnami/redis chart |
| Kafka | cloud MSK / strimzi / bitnami/kafka chart |
| OTel collector | opentelemetry-collector chart |
| Prometheus / Grafana | kube-prometheus-stack |
