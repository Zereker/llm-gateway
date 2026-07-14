#!/usr/bin/env bash

set -euo pipefail

DEMO_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$DEMO_DIR"

COMPOSE=(docker compose -p llm-gateway-quickstart -f compose.yaml)
API_KEY="sk-quickstart-llm-gateway"
PROMETHEUS_URL="http://127.0.0.1:9091"
GRAFANA_URL="http://127.0.0.1:3000"

wait_for_prometheus_query() {
  local query="$1"
  local description="$2"
  local body=""

  for ((attempt = 1; attempt <= 20; attempt++)); do
    body="$(curl -fsS --max-time 5 --get --data-urlencode "query=${query}" "${PROMETHEUS_URL}/api/v1/query" 2>/dev/null || true)"
    if [[ "$body" == *'"status":"success"'* && "$body" == *'"result":[{'* ]]; then
      return 0
    fi
    sleep 2
  done

  echo "[quickstart] Prometheus did not return ${description}" >&2
  return 1
}

wait_for_grafana_dashboard() {
  local body=""

  for ((attempt = 1; attempt <= 20; attempt++)); do
    body="$(curl -fsS --max-time 5 -u admin:admin "${GRAFANA_URL}/api/dashboards/uid/llm-gateway-runtime" 2>/dev/null || true)"
    if [[ "$body" == *'"uid":"llm-gateway-runtime"'* && "$body" == *'"title":"LLM Gateway · Runtime Overview"'* ]]; then
      return 0
    fi
    sleep 2
  done

  echo "[quickstart] Grafana did not provision the llm-gateway dashboard" >&2
  return 1
}

# Docker's builder cannot see the host Go module cache. Reuse an explicitly
# configured proxy when present, or discover the host Go setting when Go is
# installed; the demo Dockerfile's public default remains the final fallback.
if [[ -z "${GOPROXY:-}" ]] && command -v go >/dev/null 2>&1; then
  GOPROXY="$(go env GOPROXY)"
  export GOPROXY
fi

echo "[quickstart] building and starting the full stack"
"${COMPOSE[@]}" up --build -d --wait

echo "[quickstart] verifying the OpenAI-compatible request path"
response="$(curl -fsS --max-time 15 \
  -X POST http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"model":"mock-openai-model","messages":[{"role":"user","content":"Hello from the llm-gateway demo"}]}')"

if [[ ",${COMPOSE_PROFILES:-}," == *",observability,"* ]]; then
  echo "[quickstart] generating streaming traffic for TTFT metrics"
  stream_response="$(curl -fsS --no-buffer --max-time 15 \
    -X POST http://127.0.0.1:8080/v1/chat/completions \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d '{"model":"mock-openai-model","stream":true,"messages":[{"role":"user","content":"Hello from the observability smoke test"}]}')"
  if [[ "$stream_response" != *"[DONE]"* ]]; then
    echo "[quickstart] streaming verification did not receive [DONE]" >&2
    exit 1
  fi

  echo "[quickstart] verifying Prometheus metrics and Grafana provisioning"
  wait_for_prometheus_query 'up{job="llm-gateway"} == 1' 'a healthy gateway scrape target'
  wait_for_prometheus_query 'sum(llm_gateway_http_requests_total)' 'request metrics'
  wait_for_prometheus_query 'count(llm_gateway_http_request_duration_seconds_bucket)' 'latency histogram metrics'
  wait_for_prometheus_query 'count(llm_gateway_response_ttft_seconds_bucket)' 'TTFT histogram metrics'
  wait_for_grafana_dashboard
fi

echo
echo "llm-gateway quickstart is ready"
echo "  Gateway:       http://localhost:8080"
echo "  Console:       http://localhost:8081"
echo "  Console token: quickstart-admin-token"
echo "  API key:       ${API_KEY}"
if [[ ",${COMPOSE_PROFILES:-}," == *",observability,"* ]]; then
  echo "  Prometheus:    ${PROMETHEUS_URL}"
  echo "  Grafana:       ${GRAFANA_URL} (admin / admin)"
fi
echo
echo "Try it again:"
echo "  curl http://localhost:8080/v1/chat/completions -H 'Authorization: Bearer ${API_KEY}' -H 'Content-Type: application/json' -d '{\"model\":\"mock-openai-model\",\"messages\":[{\"role\":\"user\",\"content\":\"Hi!\"}]}'"
echo
echo "Response preview (${#response} bytes):"
if (( ${#response} > 600 )); then
  echo "${response:0:600}..."
else
  echo "$response"
fi
