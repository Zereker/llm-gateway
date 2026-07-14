COMPOSE = docker compose -p llm-gateway-benchmark -f compose.yaml
export GOPROXY ?= $(shell go env GOPROXY)

.PHONY: run down

run: ## Build the deterministic environment and print a JSON benchmark report
	$(COMPOSE) up --build -d --wait gateway upstream
	$(COMPOSE) run --build --rm --no-deps seed
	$(COMPOSE) run --build --rm --no-deps runner

down: ## Remove benchmark containers and volumes
	$(COMPOSE) down -v
