#!/usr/bin/env bash
# scripts/e2e-smoke.sh
#
# 纯数据面 e2e 烟测：
#
#   1. docker compose up -d           （mysql + redis + redpanda）
#   2. 等 stack healthy
#   3. go run cmd/mockupstream (bg)   假上游
#   4. go run cmd/gateway     (bg)    数据面（启动期跑 infra.Migrate）
#   5. go run scripts/seed-e2e        往 DB 写最小业务数据（account / endpoint / api_key）
#   6. curl /v1/chat/completions      期望 200 + content
#   7. cleanup（kill bg pids）
#
# 用法：
#   ./scripts/e2e-smoke.sh                # 默认 60s 超时；保留 docker stack
#   ./scripts/e2e-smoke.sh --teardown     # 跑完后 docker compose down -v
#
# 退出码：
#   0  全过
#   1  任意环节失败（详细 log 留在 /tmp/e2e-smoke-{gateway,mockupstream}.log）

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

TEARDOWN=0
for arg in "$@"; do
  case "$arg" in
    --teardown) TEARDOWN=1 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

# 临时文件
LOG_GW="/tmp/e2e-smoke-gateway.log"
LOG_UP="/tmp/e2e-smoke-mockupstream.log"
PID_GW=""
PID_UP=""

# 测试参数
API_KEY="sk-test-e2e-$RANDOM"
MODEL="gpt-4o"
DATA_KEY="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
DSN="root:@tcp(127.0.0.1:3306)/llm_gateway?parseTime=true&charset=utf8mb4"
GATEWAY_PORT=8080
MOCK_PORT=9090

cleanup() {
  local code=$?
  echo "[smoke] cleanup..." >&2
  [[ -n "$PID_GW" ]] && kill "$PID_GW" 2>/dev/null || true
  [[ -n "$PID_UP" ]] && kill "$PID_UP" 2>/dev/null || true
  wait 2>/dev/null || true
  if [[ "$TEARDOWN" == "1" ]]; then
    echo "[smoke] docker compose down -v" >&2
    docker compose down -v >/dev/null 2>&1 || true
  fi
  exit "$code"
}
trap cleanup EXIT INT TERM

# 简单的 wait-for-port 工具（避免依赖 wait-for-it）
wait_port() {
  local host="$1" port="$2" timeout="${3:-30}"
  for ((i=0; i<timeout; i++)); do
    if (echo > "/dev/tcp/$host/$port") 2>/dev/null; then
      return 0
    fi
    sleep 1
  done
  return 1
}

wait_http() {
  local url="$1" timeout="${2:-30}"
  for ((i=0; i<timeout; i++)); do
    if curl -sf -o /dev/null --max-time 2 "$url"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# ============================================================================
# 1) docker stack
# ============================================================================
echo "[smoke] docker compose up -d"
docker compose up -d >/dev/null

echo "[smoke] wait for mysql / redis"
wait_port 127.0.0.1 3306 60 || { echo "mysql 起不来" >&2; exit 1; }
wait_port 127.0.0.1 6379 30 || { echo "redis 起不来" >&2; exit 1; }
# mysqld 内部还有一段初始化时间——再 ping 一下
for ((i=0; i<30; i++)); do
  if docker compose exec -T mysql mysqladmin ping -h localhost -uroot --silent >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

# ============================================================================
# 2) mockupstream（后台）
# ============================================================================
echo "[smoke] start mockupstream :$MOCK_PORT"
MOCK_ADDR=":$MOCK_PORT" go run ./cmd/mockupstream >"$LOG_UP" 2>&1 &
PID_UP=$!
wait_http "http://127.0.0.1:$MOCK_PORT/health" 30 || {
  echo "mockupstream 启动失败，log: $LOG_UP" >&2
  cat "$LOG_UP" >&2
  exit 1
}

# ============================================================================
# 3) gateway（后台；启动期自跑 infra.Migrate）
# ============================================================================
echo "[smoke] start gateway :$GATEWAY_PORT"
go run ./cmd/gateway -config ./configs/local/gateway.yaml >"$LOG_GW" 2>&1 &
PID_GW=$!
wait_http "http://127.0.0.1:$GATEWAY_PORT/healthz" 60 || {
  echo "gateway 启动失败，log: $LOG_GW" >&2
  tail -30 "$LOG_GW" >&2
  exit 1
}

# ============================================================================
# 4) seed 业务数据
# ============================================================================
echo "[smoke] seed e2e data (api_key=$API_KEY)"
go run ./scripts/seed-e2e \
  -dsn "$DSN" \
  -data-key "$DATA_KEY" \
  -upstream "http://127.0.0.1:$MOCK_PORT/v1/chat/completions" \
  -api-key "$API_KEY" \
  -model "$MODEL" >/dev/null || {
  echo "seed 失败" >&2
  exit 1
}

# repo TTL cache 默认 30s——刚 seed 的 api_key / endpoint 第一次查肯定走 SQL，
# 但前置 health check 期间 gateway 可能已经缓存了空结果（apikey 不缓存 nil；
# subscription 缓存 false 30s）。给一个短 sleep 让 cache 自然失效。
# v0.7 起 subscription 也不缓存 false 时这里可删。
sleep 2

# ============================================================================
# 5) 发请求 → 期望 200 + content
# ============================================================================
echo "[smoke] curl /v1/chat/completions"
RESP="$(curl -sS -o /tmp/e2e-smoke-resp.json -w '%{http_code}' \
  -X POST "http://127.0.0.1:$GATEWAY_PORT/v1/chat/completions" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")"

if [[ "$RESP" != "200" ]]; then
  echo "[smoke] FAIL: HTTP $RESP" >&2
  echo "response body:" >&2
  cat /tmp/e2e-smoke-resp.json >&2
  echo "gateway log (last 30):" >&2
  tail -30 "$LOG_GW" >&2
  exit 1
fi

# 简单内容校验：mockupstream 默认吐 "hello from mockupstream..." 之类
if ! grep -q '"content"' /tmp/e2e-smoke-resp.json; then
  echo "[smoke] FAIL: response missing content field" >&2
  cat /tmp/e2e-smoke-resp.json >&2
  exit 1
fi

echo "[smoke] PASS"
echo "----- response -----"
cat /tmp/e2e-smoke-resp.json
echo
