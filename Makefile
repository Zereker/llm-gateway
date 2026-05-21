# 本地开发常用命令。CI 不依赖 Make——`go test ./...` 是真相来源。

MYSQL_DSN ?= root:@tcp(localhost:3306)/llm_gateway?parseTime=true&charset=utf8mb4

.PHONY: stack stack-stop stack-clean
stack:                  ## 起 mysql + redis + redpanda 容器（轻量；本地跑 Go 进程用）
	docker compose up -d
stack-stop:             ## 停容器（保留数据卷）
	docker compose stop
stack-clean:            ## 停容器并删数据卷（彻底重置）
	docker compose down -v

.PHONY: test test-integration build run-gateway run-mockupstream
test:                   ## 跑单元测试（SQL 测试看 MYSQL_DSN，没设就 skip）
	go test ./...

test-integration: stack ## 起 stack 后串行跑全测试（含 SQL / outbox）
	@echo "Waiting for MySQL..."
	@until docker compose exec -T mysql mysqladmin ping -h localhost -uroot --silent; do sleep 1; done
	MYSQL_DSN='$(MYSQL_DSN)' go test -p 1 ./...

build:                  ## 编译 cmd/gateway / cmd/mockupstream 到 ./bin（静态 binary，给容器用）
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/llm-gateway        ./cmd/gateway
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/llm-gateway-mockup ./cmd/mockupstream

run-gateway:            ## 跑 gateway（默认配置；启动期自跑 infra.Migrate 建表）
	go run ./cmd/gateway -config ./configs/local/gateway.yaml
run-mockupstream:       ## 跑 mock 上游（监听 :9090）
	MOCK_ADDR=:9090 go run ./cmd/mockupstream

.PHONY: help
help:                   ## 列出所有目标
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
