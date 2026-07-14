#!/usr/bin/env bash

set -euo pipefail

DEMO_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$DEMO_DIR"

COMPOSE=(docker compose -p llm-gateway-quickstart -f compose.yaml)
API_KEY="sk-quickstart-llm-gateway"

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

echo
echo "llm-gateway quickstart is ready"
echo "  Gateway:       http://localhost:8080"
echo "  Console:       http://localhost:8081"
echo "  Console token: quickstart-admin-token"
echo "  API key:       ${API_KEY}"
if [[ ",${COMPOSE_PROFILES:-}," == *",observability,"* ]]; then
  echo "  Prometheus:    http://localhost:9091"
  echo "  Grafana:       http://localhost:3000 (admin / admin)"
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
