COMPOSE = docker compose -p llm-gateway-demo -f ../demo/compose.yaml --profile benchmark

.PHONY: run down

run: ## Build the deterministic environment and print a JSON benchmark report
	$(COMPOSE) up --build -d --wait gateway benchmark-upstream
	$(COMPOSE) run --build --rm --no-deps benchmark-seed
	$(COMPOSE) run --build --rm --no-deps benchmark

down: ## Remove benchmark and shared demo containers and volumes
	$(COMPOSE) down -v
