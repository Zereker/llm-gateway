COMPOSE = docker compose -p llm-gateway-benchmark -f compose.yaml
export GOPROXY ?= $(shell go env GOPROXY)
export BENCHMARK_COMMIT_SHA ?= $(shell git rev-parse --short=12 HEAD)

.PHONY: run measure check baseline down

run: measure check ## Measure and check the result against the reference baseline

measure: ## Build the environment and write results/latest.json
	mkdir -p results
	$(COMPOSE) up --build -d --wait gateway upstream
	$(COMPOSE) run --build --rm --no-deps seed
	$(COMPOSE) run --build --rm --no-deps runner

check: ## Compare results/latest.json with the versioned reference
	$(COMPOSE) run --build --rm --no-deps compare

baseline: measure ## Replace the reference baseline with the latest local result
	cp results/latest.json baselines/reference.json

down: ## Remove benchmark containers and volumes
	$(COMPOSE) down -v
