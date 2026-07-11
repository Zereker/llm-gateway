# Common local development commands. CI does not depend on Make -- `go test ./...` is the source of truth.

# Tests run against a SEPARATE database (llm_gateway_test) so the suite's
# TRUNCATEs never wipe the llm_gateway data a developer seeds for manual / e2e
# work. The test DB is created by configs/mysql-init on a fresh volume; the
# test-integration target also creates it defensively for pre-existing volumes.
MYSQL_DSN ?= root:@tcp(localhost:3306)/llm_gateway_test?parseTime=true&charset=utf8mb4

.PHONY: stack stack-stop stack-clean
stack:                  ## Start mysql + redis + redpanda containers (lightweight; for running local Go processes)
	docker compose up -d
stack-stop:             ## Stop containers (keep data volumes)
	docker compose stop
stack-clean:            ## Stop containers and remove data volumes (full reset)
	docker compose down -v

.PHONY: test test-integration build run-gateway run-console run-migrate run-mockupstream
test:                   ## Run unit tests (SQL tests depend on MYSQL_DSN; skipped if unset)
	go test ./...

test-integration: stack ## Start the stack then run the full test suite serially (including SQL / outbox)
	@echo "Waiting for MySQL..."
	@until docker compose exec -T mysql mysqladmin ping -h localhost -uroot --silent; do sleep 1; done
	@docker compose exec -T mysql mysql -uroot -e "CREATE DATABASE IF NOT EXISTS llm_gateway_test CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"
	MYSQL_DSN='$(MYSQL_DSN)' go test -p 1 ./...

build:                  ## Compile cmd/gateway / cmd/console / cmd/mockupstream into ./bin (static binaries, for containers)
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/llm-gateway              ./cmd/gateway
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/llm-gateway-console      ./cmd/console
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/llm-gateway-migrate      ./cmd/migrate
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/llm-gateway-mockup       ./cmd/mockupstream

run-gateway:            ## Run gateway (default config; runs infra.Migrate to create tables at startup)
	go run ./cmd/gateway -config ./configs/local/gateway.yaml
run-console:            ## Run the control-plane Admin API (:8081; shares MySQL + KEK with gateway)
	go run ./cmd/console -config ./configs/local/console.yaml
run-migrate:            ## Apply versioned database migrations
	go run ./cmd/migrate -config ./configs/local/gateway.yaml
run-mockupstream:       ## Run the mock upstream (listens on :9090)
	MOCK_ADDR=:9090 go run ./cmd/mockupstream

.PHONY: smoke smoke-clean
smoke:                  ## e2e smoke test (start stack + gateway + mockupstream + seed + curl)
	./scripts/e2e-smoke.sh
smoke-clean:            ## Same as smoke but runs docker compose down -v afterward
	./scripts/e2e-smoke.sh --teardown

.PHONY: help
help:                   ## List all targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
