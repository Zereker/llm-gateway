# examples/full-config — 完整功能配置示例

跟 `configs/local/` 的"最小起步"不同，本目录展示**生产形态**的配置：
Kafka outbox + 多 model + 多 endpoint + 多 quota_policy + pricing_version。

## 文件

| 文件 | 用途 |
|---|---|
| `gateway.yaml` | 数据面（接 LLM 客户端请求）配置 |
| `admin.yaml`   | 控制面（admin REST API）配置 |
| `seed.sql`     | DB 示例数据（quota / tenant / model / pricing） |

## 启动顺序

```bash
# 1) 起本地 stack（mysql + redis + redpanda）
docker compose up -d

# 2) 建表
docker exec -i $(docker compose ps -q mysql) mysql -uroot ai_gateway < pkg/infra/schema.sql

# 3) seed 示例数据（**仅 quota_policies / tenants / model_services / subscriptions / pricing**；
#    endpoints + api_keys 必须走 admin POST 走加密路径，见下文）
docker exec -i $(docker compose ps -q mysql) mysql -uroot ai_gateway < examples/full-config/seed.sql

# 4) 起 admin
go run ./cmd/admin -config ./examples/full-config/admin.yaml &

# 5) admin 创建 endpoint（auth 列加密走这里）
curl -X POST http://localhost:8081/admin/v1/endpoints \
  -H "X-Admin-Token: CHANGEME-admin-token" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "openai-prod-1",
    "vendor": "openai",
    "model": "gpt-4o",
    "group_name": "default",
    "weight": 100,
    "auth": {"api_key": "sk-..."},
    "routing": {"url": "https://api.openai.com"}
  }'

# 6) admin 创建 api_key（hash 走这里）
curl -X POST http://localhost:8081/admin/v1/apikeys \
  -H "X-Admin-Token: CHANGEME-admin-token" \
  -H "Content-Type: application/json" \
  -d '{"tenant_id": "demo-acme", "user_id": "alice@demo-acme", "name": "alice-prod"}'
# 响应里会含明文 api_key（仅 Create 响应出现一次）；保存好它

# 7) 起 gateway
go run ./cmd/gateway -config ./examples/full-config/gateway.yaml

# 8) 调
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer <step6 拿到的 api_key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

## 跟 configs/local 的差别

| 维度 | configs/local | examples/full-config |
|---|---|---|
| outbox | file（JSONL 追加） | kafka |
| middleware.timeout | 60s | 120s |
| scheduler.max_per_endpoint | 1（无 L1 retry） | 2（L1 retry 一次） |
| seed 数据 | 无 | 多 tenant/model/pricing |
| MySQL host | localhost | mysql.internal（生产 hostname） |

## 排错

- **gateway 启动报 "schema check failed"**：先跑 `pkg/infra/schema.sql`
- **请求 401**：检查 api_key hash 是否入库（admin POST 路径）
- **请求 503 "no endpoint succeeded"**：检查 endpoint 的 auth/routing 是否配对
- **没看到 cost 事件**：检查 Kafka topic 是否存在 + `/tmp/ai-gateway-usage.log` 没在用 file 兜底
