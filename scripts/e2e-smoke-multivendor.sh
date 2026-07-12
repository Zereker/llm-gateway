#!/usr/bin/env bash
# scripts/e2e-smoke-multivendor.sh
#
# Real-binary, black-box multi-vendor smoke test -- the complement to
# fieldmatrix_multivendor_test.go's in-process e2e (internal/app/gateway):
# same idea (one endpoint + one real API key per upstream vendor, seeded
# fresh every run), but driven through the actually-compiled cmd/gateway and
# cmd/mockupstream binaries over a real network round trip, so a genuine
# process-startup / listen / real-HTTP path gets exercised too, not just gin's
# in-process test harness.
#
#   1. docker compose up -d                 (mysql + redis + redpanda)
#   2. wait for the stack to become healthy
#   3. go run cmd/mockupstream (bg)          fake upstream: openai/anthropic/gemini/cohere/azure-openai/bedrock routes
#   4. go run cmd/gateway     (bg)            data plane (runs infra.Migrate at startup)
#   5. go run scripts/seed-multivendor        one endpoint + one real api_key PER VENDOR (idempotent)
#   6. curl /v1/chat/completions or /v1/messages, once per vendor -> expect 200 + content
#   7. cleanup (kill background pids)
#
# Usage:
#   ./scripts/e2e-smoke-multivendor.sh                # default 60s timeout; keeps the docker stack
#   ./scripts/e2e-smoke-multivendor.sh --teardown     # runs docker compose down -v afterward
#   ./scripts/e2e-smoke-multivendor.sh --no-docker-stack  # assume mysql/redis are already
#                                                          # reachable on localhost (e.g. GitHub
#                                                          # Actions service containers) instead
#                                                          # of bringing up docker-compose's stack;
#                                                          # see .github/workflows/ci.yml's
#                                                          # smoke-multivendor job
#
# Exit codes:
#   0  all vendors passed (see testdata/fieldmatrix/endpoints/*.json for the current list)
#   1  some step failed (detailed logs left in /tmp/e2e-smoke-mv-{gateway,mockupstream}.log)

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

TEARDOWN=0
NO_DOCKER_STACK=0
for arg in "$@"; do
  case "$arg" in
    --teardown) TEARDOWN=1 ;;
    --no-docker-stack) NO_DOCKER_STACK=1 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

LOG_GW="/tmp/e2e-smoke-mv-gateway.log"
LOG_UP="/tmp/e2e-smoke-mv-mockupstream.log"
PID_GW=""
PID_UP=""

DATA_KEY="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
DSN="root:@tcp(127.0.0.1:3306)/llm_gateway?parseTime=true&charset=utf8mb4"
GATEWAY_PORT=8080
MOCK_PORT=9090

cleanup() {
  local code=$?
  echo "[smoke-mv] cleanup..." >&2
  [[ -n "$PID_GW" ]] && kill "$PID_GW" 2>/dev/null || true
  [[ -n "$PID_UP" ]] && kill "$PID_UP" 2>/dev/null || true
  wait 2>/dev/null || true
  if [[ "$TEARDOWN" == "1" && "$NO_DOCKER_STACK" == "0" ]]; then
    echo "[smoke-mv] docker compose down -v" >&2
    docker compose down -v >/dev/null 2>&1 || true
  fi
  exit "$code"
}
trap cleanup EXIT INT TERM

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
# 1) docker stack (skipped with --no-docker-stack: the caller already has
#    mysql/redis reachable on localhost, e.g. GitHub Actions service containers)
# ============================================================================
if [[ "$NO_DOCKER_STACK" == "0" ]]; then
  echo "[smoke-mv] docker compose up -d"
  docker compose up -d >/dev/null

  echo "[smoke-mv] wait for mysql / redis"
  wait_port 127.0.0.1 3306 60 || { echo "mysql failed to start" >&2; exit 1; }
  wait_port 127.0.0.1 6379 30 || { echo "redis failed to start" >&2; exit 1; }
  for ((i=0; i<30; i++)); do
    if docker compose exec -T mysql mysqladmin ping -h localhost -uroot --silent >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
else
  echo "[smoke-mv] --no-docker-stack: assuming mysql/redis are already up"
  wait_port 127.0.0.1 3306 60 || { echo "mysql not reachable" >&2; exit 1; }
  wait_port 127.0.0.1 6379 30 || { echo "redis not reachable" >&2; exit 1; }
fi

# ============================================================================
# 2) mockupstream (background) -- serves openai/anthropic/gemini/cohere/azure-openai/bedrock routes
# ============================================================================
echo "[smoke-mv] start mockupstream :$MOCK_PORT"
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
echo "[smoke-mv] start gateway :$GATEWAY_PORT"
go run ./cmd/gateway -config ./configs/local/gateway.yaml >"$LOG_GW" 2>&1 &
PID_GW=$!
wait_http "http://127.0.0.1:$GATEWAY_PORT/healthz" 60 || {
  echo "gateway failed to start, log: $LOG_GW" >&2
  tail -30 "$LOG_GW" >&2
  exit 1
}

# ============================================================================
# 4) seed one endpoint + one real api_key per vendor
# ============================================================================
echo "[smoke-mv] seed multi-vendor data"
go run ./scripts/seed-multivendor \
  -dsn "$DSN" \
  -data-key "$DATA_KEY" \
  -mock-base "http://127.0.0.1:$MOCK_PORT" || {
  echo "seed failed" >&2
  exit 1
}

# repo TTL cache defaults to 30s; see scripts/e2e-smoke.sh's identical note.
sleep 2

# ============================================================================
# 5) curl each vendor -> expect 200 + content
# ============================================================================
FAIL=0

check_openai_shaped() {
  local vendor="$1" resp="$2"
  if ! grep -q '"content"' <<<"$resp"; then
    echo "[smoke-mv] FAIL ($vendor): response missing content field: $resp" >&2
    FAIL=1
  fi
}

echo "[smoke-mv] curl openai (/v1/chat/completions)"
RESP="$(curl -sS -w '\n%{http_code}' -X POST "http://127.0.0.1:$GATEWAY_PORT/v1/chat/completions" \
  -H "Authorization: Bearer sk-mv-openai" -H "Content-Type: application/json" \
  -d '{"model":"mock-openai-model","messages":[{"role":"user","content":"hi"}]}')"
CODE="${RESP##*$'\n'}"; BODY="${RESP%$'\n'*}"
if [[ "$CODE" != "200" ]]; then echo "[smoke-mv] FAIL (openai): HTTP $CODE: $BODY" >&2; FAIL=1; else check_openai_shaped openai "$BODY"; fi

echo "[smoke-mv] curl anthropic (/v1/messages)"
RESP="$(curl -sS -w '\n%{http_code}' -X POST "http://127.0.0.1:$GATEWAY_PORT/v1/messages" \
  -H "Authorization: Bearer sk-mv-anthropic" -H "Content-Type: application/json" \
  -d '{"model":"mock-anthropic-model","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}')"
CODE="${RESP##*$'\n'}"; BODY="${RESP%$'\n'*}"
if [[ "$CODE" != "200" ]]; then echo "[smoke-mv] FAIL (anthropic): HTTP $CODE: $BODY" >&2; FAIL=1; else check_openai_shaped anthropic "$BODY"; fi

# gemini/cohere are upstream-only (no client-facing protocol -- see CLAUDE.md's
# "Client Protocol Scope"); clients reach them through the OpenAI chat endpoint.
echo "[smoke-mv] curl gemini (via /v1/chat/completions)"
RESP="$(curl -sS -w '\n%{http_code}' -X POST "http://127.0.0.1:$GATEWAY_PORT/v1/chat/completions" \
  -H "Authorization: Bearer sk-mv-gemini" -H "Content-Type: application/json" \
  -d '{"model":"mock-gemini-model","messages":[{"role":"user","content":"hi"}]}')"
CODE="${RESP##*$'\n'}"; BODY="${RESP%$'\n'*}"
if [[ "$CODE" != "200" ]]; then echo "[smoke-mv] FAIL (gemini): HTTP $CODE: $BODY" >&2; FAIL=1; else check_openai_shaped gemini "$BODY"; fi

echo "[smoke-mv] curl cohere (via /v1/chat/completions)"
RESP="$(curl -sS -w '\n%{http_code}' -X POST "http://127.0.0.1:$GATEWAY_PORT/v1/chat/completions" \
  -H "Authorization: Bearer sk-mv-cohere" -H "Content-Type: application/json" \
  -d '{"model":"mock-cohere-model","messages":[{"role":"user","content":"hi"}]}')"
CODE="${RESP##*$'\n'}"; BODY="${RESP%$'\n'*}"
if [[ "$CODE" != "200" ]]; then echo "[smoke-mv] FAIL (cohere): HTTP $CODE: $BODY" >&2; FAIL=1; else check_openai_shaped cohere "$BODY"; fi

# azure-openai is also upstream-only client-side (same client-facing OpenAI
# endpoint; only the upstream URL shape + api-key header differ, both handled
# entirely on the gateway side).
echo "[smoke-mv] curl azure-openai (via /v1/chat/completions)"
RESP="$(curl -sS -w '\n%{http_code}' -X POST "http://127.0.0.1:$GATEWAY_PORT/v1/chat/completions" \
  -H "Authorization: Bearer sk-mv-azure-openai" -H "Content-Type: application/json" \
  -d '{"model":"mock-azure-openai-model","messages":[{"role":"user","content":"hi"}]}')"
CODE="${RESP##*$'\n'}"; BODY="${RESP%$'\n'*}"
if [[ "$CODE" != "200" ]]; then echo "[smoke-mv] FAIL (azure-openai): HTTP $CODE: $BODY" >&2; FAIL=1; else check_openai_shaped azure-openai "$BODY"; fi

# bedrock (Converse API) is also upstream-only client-side.
echo "[smoke-mv] curl bedrock (via /v1/chat/completions)"
RESP="$(curl -sS -w '\n%{http_code}' -X POST "http://127.0.0.1:$GATEWAY_PORT/v1/chat/completions" \
  -H "Authorization: Bearer sk-mv-bedrock" -H "Content-Type: application/json" \
  -d '{"model":"mock-bedrock-model","messages":[{"role":"user","content":"hi"}]}')"
CODE="${RESP##*$'\n'}"; BODY="${RESP%$'\n'*}"
if [[ "$CODE" != "200" ]]; then echo "[smoke-mv] FAIL (bedrock): HTTP $CODE: $BODY" >&2; FAIL=1; else check_openai_shaped bedrock "$BODY"; fi

if [[ "$FAIL" != "0" ]]; then
  echo "[smoke-mv] one or more vendors FAILED; gateway log (last 30):" >&2
  tail -30 "$LOG_GW" >&2
  exit 1
fi

echo "[smoke-mv] PASS (openai, anthropic, gemini, cohere, azure-openai, bedrock)"
