# examples/full-config — Full-featured config example

Unlike the "minimal starting point" of `configs/local/`, this directory demonstrates a **production-shaped** configuration:
Kafka outbox + multiple models + multiple endpoints + multiple quota_policies + pricing_version.

## Files

| File | Purpose |
|---|---|
| `gateway.yaml` | Data plane (handles LLM client requests) config |
| `seed.sql`     | DB sample data (quota / account / model / pricing, etc.) |

## Startup order

```bash
# 1) Start the local stack (mysql + redis + redpanda)
docker compose up -d

# 2) Start gateway (infra.Migrate runs automatically at startup to create tables)
go run ./cmd/gateway -config ./examples/full-config/gateway.yaml

# 3) Seed sample data (quota_policies / accounts / model_services / subscriptions /
#    pricing). The encrypted / hash columns for endpoints + api_keys need to be
#    computed yourself via a script or by referencing EncodePayload / HashAPIKey
#    in pkg/repo to generate the ciphertext / hash before INSERTing.
docker exec -i $(docker compose ps -q mysql) mysql -uroot llm_gateway < examples/full-config/seed.sql

# 4) Try it
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer <plaintext api_key you hashed + inserted yourself>" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

## Data management

This project is **data plane only** — it does not provide a control plane REST API. Business tables (accounts / model_services /
endpoints / api_keys / quota_policies / subscriptions / pricing_versions) are maintained by the
deployer via direct SQL insert / update / delete.

Handling of encrypted / hash columns:

- **endpoints.auth**: AuthConfig encrypted with AES-256-GCM; compute the ciphertext (`v1:base64...`) with `repo.EncodePayload` +
  `repo.SetDataKey(cfg.DataKey)`, then INSERT it.
- **api_keys.api_key_hash**: compute the SHA-256 hex with `repo.HashAPIKey(plaintext)`;
  the plaintext is never stored in the DB, and is given to the user to keep.

After data is written, the gateway gradually picks up the new values through the repo layer's in-process TTL LRU cache (30s by default).
The deployer does not need to perform any invalidation; accept this delay, since business table changes don't need to take effect within seconds.

## Differences from configs/local

| Dimension | configs/local | examples/full-config |
|---|---|---|
| outbox | file (JSONL append) | kafka |
| middleware.timeout | 60s | 120s |
| scheduler.max_attempts | 3 | 3 |
| seed data | none | multiple accounts/models/pricing |
| MySQL host | localhost | mysql.internal (production hostname) |

## Troubleshooting

- **gateway startup reports "schema check failed"**: gateway runs `infra.Migrate` at startup;
  if MySQL permissions are insufficient to create tables, run `pkg/infra/schema.sql` manually
- **request returns 401**: check whether `api_keys.api_key_hash` matches the client's `Authorization` header
  as computed by `repo.HashAPIKey()`
- **request returns 503 "no endpoint succeeded"**: check whether the endpoint's auth/routing are paired correctly,
  and whether endpoint.protocol matches the client protocol (a field added in v0.6)
- **no usage event seen**: check whether the Kafka topic exists, or confirm whether it was switched back to file outbox
