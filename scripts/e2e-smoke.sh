#!/bin/bash
# e2e-smoke.sh — 端到端冒烟测试。
#
# 假设：docker compose --profile e2e up -d 已成功（admin/gateway/mockupstream/flink 都 healthy）
#
# 流程：
#   1. admin POST /admin/v1/quota-policies            — 建 quota policy
#   2. admin POST /admin/v1/accounts                  — 建主账号 demo-acme
#   3. admin POST /admin/v1/modelservices             — 注册 model（gpt-4o）
#   4. admin POST /admin/v1/accounts/.../subscriptions — 主账号订阅 model
#   5. admin POST /admin/v1/modelservices/.../prices  — 上 pricing version
#   6. admin POST /admin/v1/endpoints                 — 注册上游 endpoint（指向 mockupstream）
#   7. admin POST /admin/v1/apikeys                   — 建 api_key（明文一次性返）
#   8. 等 Debezium 把变更同步到 Redis Stream（gateway L1 cache 失效）
#   9. curl gateway /v1/chat/completions              — 真正的请求走完 M1-M10
#  10. 校验 gateway 卷里 usage.jsonl 有事件             — outbox 落地
#  11. 校验 Kafka topic 有事件                          — Kafka 投递
#  12. 等 Flink 出 billing batch                       — log sink JSONL
#
# 任何一步失败即 exit 非 0。

set -euo pipefail

ADMIN_URL="${ADMIN_URL:-http://localhost:8081}"
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
ADMIN_TOKEN="${ADMIN_TOKEN:-local-dev-token}"

JQ="${JQ:-jq}"
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1"; exit 2; }; }
need curl
need "$JQ"

a() {
    # admin helper: a METHOD PATH [JSON]
    local m="$1" p="$2" body="${3:-}"
    if [ -n "$body" ]; then
        curl -sS -X "$m" "${ADMIN_URL}${p}" \
            -H "X-Admin-Token: ${ADMIN_TOKEN}" \
            -H "Content-Type: application/json" \
            -d "$body"
    else
        curl -sS -X "$m" "${ADMIN_URL}${p}" \
            -H "X-Admin-Token: ${ADMIN_TOKEN}"
    fi
}

section() { printf "\n========== %s ==========\n" "$*"; }

section "0. preflight"
curl -fsS "${ADMIN_URL}/healthz" >/dev/null && echo "  admin OK"
curl -fsS "${GATEWAY_URL}/healthz" >/dev/null && echo "  gateway OK"

section "1. quota policy"
QP_RAW="$(a POST /admin/v1/quota-policies '{
  "name": "e2e-tier1",
  "description": "e2e smoke",
  "rule_json": {"default": {"rpm": 600, "tpm": 1000000}},
  "enabled": true
}')"
QP_ID="$(echo "$QP_RAW" | $JQ -r '.id')"
[ -n "$QP_ID" ] && [ "$QP_ID" != "null" ] || { echo "FAIL: $QP_RAW"; exit 1; }
echo "  quota_policy id=${QP_ID}"

section "2. account"
a POST /admin/v1/accounts "{
  \"pin\": \"demo-acme\",
  \"name\": \"ACME (e2e)\",
  \"enabled\": true,
  \"quota_policy_id\": ${QP_ID}
}" > /dev/null
echo "  account pin=demo-acme"

section "3. model service"
MS_RAW="$(a POST /admin/v1/modelservices '{
  "service_id": "openai/gpt-4o",
  "model": "gpt-4o"
}')"
MS_ID="$(echo "$MS_RAW" | $JQ -r '.id')"
[ -n "$MS_ID" ] && [ "$MS_ID" != "null" ] || { echo "FAIL: $MS_RAW"; exit 1; }
echo "  model_service id=${MS_ID}"

section "4. subscription"
a POST "/admin/v1/accounts/demo-acme/subscriptions" "{
  \"model_service_id\": ${MS_ID}
}" > /dev/null
echo "  subscribed demo-acme → ${MS_ID}"

section "5. pricing"
a POST "/admin/v1/modelservices/gpt-4o/prices?account_id=demo-acme" '{
  "rule_class": "standard",
  "rule_json": {
    "BaseUnit": "1K_tokens",
    "Rates": {"Input": 0.0025, "Output": 0.01},
    "ModelRatio": 1.0
  },
  "notes": "e2e"
}' > /dev/null
echo "  pricing version published"

section "6. endpoint (→ mockupstream)"
a POST /admin/v1/endpoints '{
  "name": "mock-openai",
  "vendor": "openai",
  "model": "gpt-4o",
  "group": "default",
  "weight": 100,
  "enabled": true,
  "auth": {"type": "bearer", "payload": {"api_key": "sk-mock"}},
  "routing": {"url": "http://mockupstream:9090/v1/chat/completions"}
}' > /dev/null
echo "  endpoint mock-openai registered"

section "7. api_key"
AK_RAW="$(a POST /admin/v1/apikeys "{
  \"account_id\": \"demo-acme\",
  \"sub_account_id\": \"alice@demo-acme\",
  \"name\": \"e2e-alice\",
  \"group\": \"default\",
  \"quota_policy_id\": ${QP_ID}
}")"
API_KEY="$(echo "$AK_RAW" | $JQ -r '.api_key')"
[ -n "$API_KEY" ] && [ "$API_KEY" != "null" ] || { echo "no api_key in response: $AK_RAW"; exit 1; }
echo "  api_key (mask) ${API_KEY:0:12}*****"

section "8. wait for Debezium → Redis cache invalidation (sleep 6s)"
sleep 6

section "9. gateway /v1/chat/completions"
RESP="$(curl -sS -X POST "${GATEWAY_URL}/v1/chat/completions" \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}')"
echo "$RESP" | $JQ .
TOTAL="$(echo "$RESP" | $JQ -r '.usage.total_tokens')"
[ "$TOTAL" = "20" ] || { echo "unexpected usage in gateway response (total_tokens=$TOTAL)"; exit 1; }

section "10. verify usage.jsonl outbox file"
LINES="$(docker compose exec -T gateway sh -c 'wc -l < /var/lib/llm-gateway/usage.jsonl 2>/dev/null || echo 0')"
echo "  outbox lines=${LINES}"
[ "${LINES// /}" -ge 1 ] || { echo "usage outbox empty"; exit 1; }

section "11. verify Kafka topic billing.usage.recorded.v1 has events"
docker compose exec -T redpanda rpk topic consume billing.usage.recorded.v1 -n 1 --offset start 2>/dev/null \
    | $JQ -r '.value' | head -c 400
echo

section "12. wait for Flink billing batch (up to 90s)"
deadline=$(( $(date +%s) + 90 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
    if docker compose exec -T flink-taskmanager sh -c \
        '[ -s /var/log/billing-aggregator/batches.jsonl ]' 2>/dev/null; then
        echo "  batches.jsonl populated:"
        docker compose exec -T flink-taskmanager head -c 400 /var/log/billing-aggregator/batches.jsonl
        echo
        echo
        echo "✓ E2E smoke PASSED"
        exit 0
    fi
    sleep 5
done
echo "✗ Flink batch never landed within 90s"
exit 1
