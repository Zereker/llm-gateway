.PHONY: up observe down logs

up: ## Build, start, seed, and verify the self-contained demo
	./up.sh

observe: ## Start the demo with Prometheus and Grafana
	COMPOSE_PROFILES=observability ./up.sh

down: ## Stop the demo and remove its data volumes
	docker compose -p llm-gateway-quickstart -f compose.yaml down -v

logs: ## Follow all demo service logs
	docker compose -p llm-gateway-quickstart -f compose.yaml logs -f
