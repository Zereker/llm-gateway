# Stable repository entry points. Scenario-specific implementation stays in
# examples/.

DEV_COMPOSE = docker compose -p llm-gateway-dev -f examples/local/compose.yaml
MYSQL_DSN ?= root:@tcp(localhost:3306)/llm_gateway_test?parseTime=true&charset=utf8mb4
BASE_IMAGE_REGISTRY ?= docker.io/library
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PACKAGE = github.com/zereker/llm-gateway/internal/version
LDFLAGS = -s -w -X $(VERSION_PACKAGE).Version=$(VERSION) -X $(VERSION_PACKAGE).Commit=$(COMMIT) -X $(VERSION_PACKAGE).BuildDate=$(BUILD_DATE)
RELEASE_TARGETS = linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: dev-up dev-stop dev-clean
dev-up: ## Start local MySQL, Redis, and Redpanda
	$(DEV_COMPOSE) up -d
dev-stop: ## Stop local development infrastructure
	$(DEV_COMPOSE) stop
dev-clean: ## Remove local development infrastructure and volumes
	$(DEV_COMPOSE) down -v

.PHONY: test test-integration cover
test: ## Run the complete Go test suite
	go test ./...
test-integration: dev-up ## Run tests with the local SQL infrastructure
	@until $(DEV_COMPOSE) exec -T mysql mysqladmin ping -h localhost -uroot --silent; do sleep 1; done
	@$(DEV_COMPOSE) exec -T mysql mysql -uroot -e "CREATE DATABASE IF NOT EXISTS llm_gateway_test CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"
	MYSQL_DSN='$(MYSQL_DSN)' go test -p 1 ./...
cover: ## Generate coverage for all internal packages
	go test -coverprofile=coverage.txt ./internal/...
	go tool cover -func=coverage.txt | tail -1

.PHONY: build docker-build release-snapshot
build: ## Build the two production commands
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/llm-gateway ./cmd/gateway
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/llm-gateway-console ./cmd/console
docker-build: ## Build the production gateway image
	docker build --build-arg BASE_IMAGE_REGISTRY="$(BASE_IMAGE_REGISTRY)" --build-arg GOPROXY="$$(go env GOPROXY)" --build-arg VERSION="$(VERSION)" --build-arg COMMIT="$(COMMIT)" --build-arg BUILD_DATE="$(BUILD_DATE)" --target gateway -t llm-gateway:local .
release-snapshot: ## Build checksummed gateway + console archives for every release platform
	rm -rf dist
	mkdir -p dist
	@set -eu; for target in $(RELEASE_TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		name="llm-gateway_$(VERSION)_$${os}_$${arch}"; dir="dist/$${name}"; ext=""; \
		if [ "$${os}" = windows ]; then ext=.exe; fi; \
		mkdir -p "$${dir}"; \
		CGO_ENABLED=0 GOOS="$${os}" GOARCH="$${arch}" go build -trimpath -ldflags="$(LDFLAGS)" -o "$${dir}/llm-gateway$${ext}" ./cmd/gateway; \
		CGO_ENABLED=0 GOOS="$${os}" GOARCH="$${arch}" go build -trimpath -ldflags="$(LDFLAGS)" -o "$${dir}/llm-gateway-console$${ext}" ./cmd/console; \
		cp LICENSE "$${dir}/LICENSE"; \
		mkdir -p "$${dir}/configs"; cp deploy/configs/*.yaml "$${dir}/configs/"; \
		cp docs/INSTALL.md docs/INSTALL.zh-CN.md "$${dir}/"; \
		if [ "$${os}" = windows ]; then (cd dist && zip -qr "$${name}.zip" "$${name}"); else tar -C dist -czf "dist/$${name}.tar.gz" "$${name}"; fi; \
		rm -rf "$${dir}"; \
	done
	@cd dist && if command -v sha256sum >/dev/null 2>&1; then sha256sum *.tar.gz *.zip > SHA256SUMS; else shasum -a 256 *.tar.gz *.zip > SHA256SUMS; fi

.PHONY: run-gateway run-console run-mockupstream
run-gateway: ## Run the data plane with the local development config
	go run ./cmd/gateway -config ./examples/local/configs/gateway.yaml
run-console: ## Run the control plane with the local development config
	go run ./cmd/console -config ./examples/local/configs/console.yaml
run-mockupstream: ## Run the shared development/test upstream
	MOCK_ADDR=:9090 go run ./examples/support/mockupstream

.PHONY: e2e e2e-clean e2e-multivendor e2e-multivendor-clean seed-fieldmatrix
e2e: ## Run the single-provider black-box smoke test
	./examples/support/e2e/smoke.sh
e2e-clean: ## Run single-provider smoke and remove its infrastructure
	./examples/support/e2e/smoke.sh --teardown
e2e-multivendor: ## Run the field-matrix black-box smoke test
	./examples/support/e2e/smoke-multivendor.sh
e2e-multivendor-clean: ## Run field-matrix smoke and remove its infrastructure
	./examples/support/e2e/smoke-multivendor.sh --teardown
seed-fieldmatrix: ## Seed all recorded provider fixtures into the local database
	go run ./examples/support/seed-fieldmatrix -dsn "root:@tcp(localhost:3306)/llm_gateway?parseTime=true&charset=utf8mb4" -data-key "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" -mock-base "http://127.0.0.1:9090"

.PHONY: quickstart quickstart-observe quickstart-down benchmark benchmark-down
quickstart: ## Start and verify the self-contained product quickstart
	$(MAKE) -C examples/quickstart up
quickstart-observe: ## Start quickstart with Prometheus and Grafana
	$(MAKE) -C examples/quickstart observe
quickstart-down: ## Remove the quickstart environment
	$(MAKE) -C examples/quickstart down
benchmark: ## Run the reproducible direct-versus-gateway benchmark
	$(MAKE) -C examples/benchmark run
benchmark-down: ## Remove the benchmark environment
	$(MAKE) -C examples/benchmark down

.PHONY: help
help: ## List repository commands
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2}'
