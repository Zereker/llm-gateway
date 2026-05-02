# 本地开发常用命令。CI 不依赖 Make——`go test ./...` 是真相来源。

MYSQL_DSN ?= root:@tcp(localhost:3306)/ai_gateway?parseTime=true&charset=utf8mb4

.PHONY: stack stack-stop stack-clean
stack:                  ## 起 mysql + redis + redpanda 容器
	docker compose up -d
stack-stop:             ## 停容器（保留数据卷）
	docker compose stop
stack-clean:            ## 停容器并删数据卷（彻底重置）
	docker compose down -v

.PHONY: test test-integration build run-gateway run-admin
test:                   ## 跑单元测试（SQL 测试看 MYSQL_DSN，没设就 skip）
	go test ./...

test-integration: stack ## 起 stack 后串行跑全测试（含 SQL / outbox）
	@echo "Waiting for MySQL..."
	@until docker compose exec -T mysql mysqladmin ping -h localhost -uroot --silent; do sleep 1; done
	MYSQL_DSN='$(MYSQL_DSN)' go test -p 1 ./...

build:                  ## 编译 cmd/gateway 和 cmd/admin 到 ./bin
	mkdir -p bin
	go build -o bin/ai-gateway       ./cmd/gateway
	go build -o bin/ai-gateway-admin ./cmd/admin

run-gateway:            ## 跑 gateway（默认配置）
	go run ./cmd/gateway -config ./configs/local/gateway.yaml

run-admin:              ## 跑 admin（默认配置）
	go run ./cmd/admin -config ./configs/local/admin.yaml

.PHONY: help
help:                   ## 列出所有目标
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
