#!/usr/bin/env bash
# scripts/e2e-smoke.sh
#
# Data-plane-only e2e smoke test:
#
#   1. docker compose up -d           (mysql + redis + redpanda)
#   2. wait for the stack to become healthy
#   3. go run cmd/mockupstream (bg)   fake upstream
#   4. go run cmd/gateway     (bg)    data plane (runs infra.Migrate at startup)
#   5. go run scripts/seed-e2e        writes minimal business data to the DB (account / endpoint / api_key)
#   6. curl /v1/chat/completions      expect 200 + content
#   7. cleanup (kill background pids)
#
# Usage:
#   ./scripts/e2e-smoke.sh                # default 60s timeout; keeps the docker stack
#   ./scripts/e2e-smoke.sh --teardown     # runs docker compose down -v afterward
#
# Exit codes:
#   0  everything passed
#   1  some step failed (detailed logs left in /tmp/e2e-smoke-{gateway,mockupstream}.log)

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

# Temp files
LOG_GW="/tmp/e2e-smoke-gateway.log"
LOG_UP="/tmp/e2e-smoke-mockupstream.log"
PID_GW=""
PID_UP=""

# Test parameters
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

# A simple wait-for-port helper (avoids depending on wait-for-it)
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
wait_port 127.0.0.1 3306 60 || { echo "mysql failed to start" >&2; exit 1; }
wait_port 127.0.0.1 6379 30 || { echo "redis failed to start" >&2; exit 1; }
# mysqld still needs some internal init time -- ping it once more
for ((i=0; i<30; i++)); do
  if docker compose exec -T mysql mysqladmin ping -h localhost -uroot --silent >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

# ============================================================================
# 2) mockupstream (background)
# ============================================================================
echo "[smoke] start mockupstream :$MOCK_PORT"
MOCK_ADDR=":$MOCK_PORT" go run ./cmd/mockupstream >"$LOG_UP" 2>&1 &
PID_UP=$!
wait_http "http://127.0.0.1:$MOCK_PORT/health" 30 || {
  echo "mockupstream failed to start, log: $LOG_UP" >&2
  cat "$LOG_UP" >&2
  exit 1
}

# ============================================================================
# 3) gateway (background; runs infra.Migrate at startup)
# ============================================================================
echo "[smoke] start gateway :$GATEWAY_PORT"
go run ./cmd/gateway -config ./configs/local/gateway.yaml >"$LOG_GW" 2>&1 &
PID_GW=$!
wait_http "http://127.0.0.1:$GATEWAY_PORT/healthz" 60 || {
  echo "gateway failed to start, log: $LOG_GW" >&2
  tail -30 "$LOG_GW" >&2
  exit 1
}

# ============================================================================
# 4) seed business data
# ============================================================================
echo "[smoke] seed e2e data (api_key=$API_KEY)"
go run ./scripts/seed-e2e \
  -dsn "$DSN" \
  -data-key "$DATA_KEY" \
  -upstream "http://127.0.0.1:$MOCK_PORT/v1/chat/completions" \
  -api-key "$API_KEY" \
  -model "$MODEL" >/dev/null || {
  echo "seed failed" >&2
  exit 1
}

# repo TTL cache defaults to 30s -- the freshly-seeded api_key / endpoint will definitely
# hit SQL on first lookup, but during the preceding health checks gateway may already have
# cached an empty result (apikey doesn't cache nil; subscription caches false for 30s).
# Add a short sleep to let the cache naturally expire.
# As of v0.7, subscription no longer caches false, so this can be removed then.
sleep 2

# ============================================================================
# 5) send request -> expect 200 + content
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

# Simple content check: mockupstream responds with something like "hello from mockupstream..." by default
if ! grep -q '"content"' /tmp/e2e-smoke-resp.json; then
  echo "[smoke] FAIL: response missing content field" >&2
  cat /tmp/e2e-smoke-resp.json >&2
  exit 1
fi

echo "[smoke] PASS"
echo "----- response -----"
cat /tmp/e2e-smoke-resp.json
echo
