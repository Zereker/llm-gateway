# 本地开发常用命令。CI 不依赖 Make——`go test ./...` 是真相来源。

MYSQL_DSN ?= root:@tcp(localhost:3306)/llm_gateway?parseTime=true&charset=utf8mb4

.PHONY: stack stack-stop stack-clean
stack:                  ## 起 mysql + redis + redpanda 容器（轻量；本地跑 Go 进程用）
	docker compose up -d
stack-stop:             ## 停容器（保留数据卷）
	docker compose stop
stack-clean:            ## 停容器并删数据卷（彻底重置）
	docker compose down -v

.PHONY: test test-integration build run-gateway run-admin run-mockupstream
test:                   ## 跑单元测试（SQL 测试看 MYSQL_DSN，没设就 skip）
	go test ./...

test-integration: stack ## 起 stack 后串行跑全测试（含 SQL / outbox）
	@echo "Waiting for MySQL..."
	@until docker compose exec -T mysql mysqladmin ping -h localhost -uroot --silent; do sleep 1; done
	MYSQL_DSN='$(MYSQL_DSN)' go test -p 1 ./...

build:                  ## 编译 cmd/gateway / cmd/admin / cmd/mockupstream 到 ./bin
	mkdir -p bin
	go build -o bin/llm-gateway        ./cmd/gateway
	go build -o bin/llm-gateway-admin  ./cmd/admin
	go build -o bin/llm-gateway-mockup ./cmd/mockupstream

run-gateway:            ## 跑 gateway（默认配置）
	go run ./cmd/gateway -config ./configs/local/gateway.yaml
run-admin:              ## 跑 admin（默认配置）
	go run ./cmd/admin -config ./configs/local/admin.yaml
run-mockupstream:       ## 跑 mock 上游（监听 :9090）
	MOCK_ADDR=:9090 go run ./cmd/mockupstream

# ============================================================================
# E2E：admin + gateway + mockupstream + nacos + flink 全栈，含计费聚合管线
# ============================================================================

.PHONY: e2e e2e-up e2e-down e2e-smoke e2e-logs e2e-status flink-jar
e2e: e2e-up e2e-smoke   ## 全栈起 + 自动跑冒烟测试（一键 E2E）

e2e-up:                 ## 起 e2e profile：admin/gateway/mockupstream/nacos/flink
	# --wait 等所有 service healthy / 一次性服务成功 exit；首跑 mvn 拉依赖较慢，超时给 10min
	docker compose --profile e2e up -d --build --wait --wait-timeout 600
	@echo
	@echo "服务清单："
	@docker compose --profile e2e ps

e2e-down:               ## 全清 e2e 栈 + 卷
	docker compose --profile e2e down -v

e2e-smoke:              ## 跑冒烟测试（前提：栈已 up）
	./scripts/e2e-smoke.sh

e2e-logs:               ## 跟踪 admin / gateway / flink 日志
	docker compose --profile e2e logs -f admin gateway flink-jobmanager flink-taskmanager

e2e-status:             ## 显示 e2e 栈所有服务状态
	docker compose --profile e2e ps

flink-jar:              ## 仅构建 Flink fat-jar（不依赖宿主 JDK，用 maven 容器）
	docker compose --profile e2e run --rm flink-jar-builder

.PHONY: help
help:                   ## 列出所有目标
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
