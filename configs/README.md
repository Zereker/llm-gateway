# configs/

Each subdirectory is a **complete, self-contained** gateway configuration, corresponding to one environment.

## Structure

```
configs/
├── local/                  # Local development config; depends on docker compose's MySQL + Redis + Redpanda
│   └── gateway.yaml
│
├── prod/                   # Production template
│   └── gateway.yaml
│
├── docker/                 # Containerized config for docker-compose (only referenced inside the image)
│   └── gateway.yaml
│
└── mysql-init/             # MySQL container init scripts (default parameters)
```

This project's target architecture **does not provide zero-external-dependency startup**. The gateway requires SQL DB and Redis to start:

- SQL DB stores business tables such as accounts / api_keys / model_services / subscriptions / endpoints /
  quota_policies / pricing_versions.
- Redis backs M6 rate limit buckets and scheduler cooldown state.
- Kafka/Redpanda is only required when the outbox driver is set to kafka; file outbox can be used for local debugging.

Schema changes are versioned in `internal/infra` and applied with `cmd/migrate`.
The local/docker configs set `database.auto_migrate: true` for convenience;
production should run the migration command or Helm migration Job first.
Gateway startup verifies the migration version and then runs `repo.CheckSchema`.

Business data can be written directly via SQL or managed through the optional
`cmd/console` Admin API. The gateway data plane never depends on the console.

## Path Resolution

External dependency addresses in the config file should point to the current environment's SQL DB, Redis, and outbox backend.
In production, do not commit real credentials to git; inject them via Secrets, environment variable rendering, or the deployment system.

## Gateway Config Structure

Top-level structure of `gateway.yaml`:

```yaml
server:
  addr: ":8080"
  read_header_timeout: 10s
  shutdown_timeout: 30s

request:
  body_limit_bytes: 10485760
  timeout: 60s

database:
  driver: mysql
  dsn: "user:pass@tcp(mysql:3306)/llm_gateway?parseTime=true&charset=utf8mb4"
  auto_migrate: false # true only for local development

redis:
  addr: "redis:6379"
  db: 0
  password: ""

data_key: "<hex-encoded-32-byte-key>"

usage_events:
  driver: file # file | kafka | async_kafka | file_and_kafka
  file:
    path: /tmp/llm-gateway-usage.log
  kafka:
    brokers: ["kafka:9092"]
    topic: billing.usage.recorded.v1
    async: true
    buffer_size: 1024
    max_retries: 3
    dlq_topic: billing.usage.recorded.v1.dlq

selector:
  filters: [cooldown, limit_read, weighted_random]
  max_attempts: 3
  cooldown:
    transient: 30s
    capacity: 60s
    permanent: 5m
    invalid: 0s
    unknown: 10s

budget:
  driver: alwayspass # alwayspass | inmemory
  default_balance: 0

moderation:
  driver: none # none | openai
  api_key: ""
  base_url: ""

trace:
  driver: slog # slog | otel
  endpoint: ""
  service_name: llm-gateway
```

When adding new config fields, you need to update `internal/config`, the example config, this file, and the corresponding architecture doc in sync.

## Adding a New Environment

```sh
cp -r configs/local configs/staging
# Edit configs/staging/gateway.yaml
go run ./cmd/gateway -config ./configs/staging/gateway.yaml
# After startup, use SQL INSERT to populate business data (see the "Data Management" section of examples/full-config/README.md)
```

## Secret Management

**Never commit real secrets to git.** Recommended:

| Scenario | Approach |
|------|------|
| local dev | Use docker compose to start MySQL / Redis / Redpanda; test endpoint/API keys via direct SQL INSERT |
| CI / staging | Independent SQL DB / Redis; secrets injected by CI secret store or deployment system |
| prod | SQL DB / Redis / Kafka use managed services; credentials go through Vault / secret manager / K8s Secret |

The DSN, Redis password, data key, and upstream API keys in `prod/gateway.yaml` must all be injected by the deployment environment,
and real values should not be committed.
