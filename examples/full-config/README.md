# examples/full-config — 完整功能配置示例

跟 `configs/local/` 的"最小起步"不同，本目录展示**生产形态**的配置：
Kafka outbox + 多 model + 多 endpoint + 多 quota_policy + pricing_version。

## 文件

| 文件 | 用途 |
|---|---|
| `gateway.yaml` | 数据面（接 LLM 客户端请求）配置 |
| `seed.sql`     | DB 示例数据（quota / account / model / pricing 等） |

## 启动顺序

```bash
# 1) 起本地 stack（mysql + redis + redpanda + debezium）
docker compose up -d

# 2) 起 gateway（启动期自跑 infra.Migrate 建表）
go run ./cmd/gateway -config ./examples/full-config/gateway.yaml

# 3) seed 示例数据（quota_policies / accounts / model_services / subscriptions /
#    pricing）。endpoints + api_keys 的加密 / hash 列需要自己用脚本算或参考
#    pkg/repo 里的 EncodePayload / HashAPIKey 自己生成密文 / hash 再 INSERT。
docker exec -i $(docker compose ps -q mysql) mysql -uroot llm_gateway < examples/full-config/seed.sql

# 4) 调
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer <自己 hash + 入库的 api_key 明文>" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

## 数据管理

本项目**只做数据面**——不提供控制面 REST API。业务表（accounts / model_services /
endpoints / api_keys / quota_policies / subscriptions / pricing_versions）由
deployer 直接 SQL 插入 / 更新 / 删除维护。

加密 / hash 列的处理：

- **endpoints.auth**：AES-256-GCM 加密的 AuthConfig；用 `repo.EncodePayload` +
  `repo.SetDataKey(cfg.DataKey)` 算出密文（`v1:base64...`）再 INSERT。
- **api_keys.api_key_hash**：`repo.HashAPIKey(plaintext)` 算 SHA-256 hex；
  明文不入库，发给用户保管。

数据写入后 Debezium binlog CDC 自动推 Redis Stream，gateway L1 cache 实时失效。

## 跟 configs/local 的差别

| 维度 | configs/local | examples/full-config |
|---|---|---|
| outbox | file（JSONL 追加） | kafka |
| middleware.timeout | 60s | 120s |
| scheduler.max_attempts | 3 | 3 |
| seed 数据 | 无 | 多 account/model/pricing |
| MySQL host | localhost | mysql.internal（生产 hostname） |

## 排错

- **gateway 启动报 "schema check failed"**：gateway 启动期跑 `infra.Migrate`；
  如果 MySQL 权限不足建不了表，手动跑 `pkg/infra/schema.sql`
- **请求 401**：检查 `api_keys.api_key_hash` 是否跟客户端 `Authorization` header
  的 `repo.HashAPIKey()` 一致
- **请求 503 "no endpoint succeeded"**：检查 endpoint 的 auth/routing 是否配对，
  endpoint.protocol 是否跟客户端协议匹配（v0.6 加的字段）
- **没看到 usage event**：检查 Kafka topic 是否存在，或确认是否切回了 file outbox
