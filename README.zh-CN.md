# llm-gateway

[English](README.md) | [简体中文](README.zh-CN.md)

一个基于 Go 实现的网关，将 LLM API 请求路由到多个上游供应商
（OpenAI、Anthropic、Google、AWS Bedrock、自建的 vLLM / Ollama 等），
对外统一暴露 OpenAI 兼容接口。

## 状态

一个 LLM API 网关的 Go 实现。架构目标记录在
[`docs/architecture`](docs/architecture/) 中。

## 数据面结构

```
HTTP request
  │
  ▼
internal/middleware        ── 请求生命周期（M1-M10）：auth / envelope / budget /
                         catalog / ratelimit / moderation / **M7 → dispatch** /
                         tracing。每个 middleware 单一职责，配 OTel option。
  │
  ▼  (M7 是 dispatch.Dispatcher 的 thin adapter)
internal/dispatch          ── 调度执行时序的**唯一**所有者：
                         CandidateSource → filterEligible → Selector.Pick →
                         EndpointQuota.Reserve → Handler lookup → Invoker →
                         Selector.Report → RetryPolicy → Stream / Charge
                         （internal/dispatch/adapters/ 桥接 selector / invoker /
                         ratelimit / repo primitives → dispatch ports）
  │
  ├── internal/selector    ── primitives：filters / scorer / picker / cooldown。
  │                      只做选择算法，不知道 protocol / handler / middleware
  ├── internal/invoker     ── primitives：一次 HTTP 调用 + 响应 forward，不做调度
  ├── internal/ratelimit   ── primitives：Store / Bucket / endpoint bucket helpers
  └── internal/protocol    ── facade：Handler = Factory + Translator + Quirks
                         消费侧只看 Handler / Lookup；Factory / Session 是内部
       ├── protocol/<vendor>/  OpenAI(+ark alias) / Anthropic / Gemini Factory + Session
       ├── protocol/quirks/    endpoint 级 body+header 微调 DSL（rename/strip/set/set_default）
       └── translator (internal/)   body shape 转换：identity + 跨协议 pairs（internal/builtin 显式装配）

internal/moderation        ── Moderator + 响应流装饰器 + ctx helpers
internal/usage             ── usage 提取 + outbox（file | kafka）；计价在下游完成
internal/trace / metric    ── Tracer 抽象（slog / OTel）+ Prom metric name 常量

internal/repo              ── data access：sqlx Reader/Provider + TTL LRU cache wrapper
                         （5 个 cached wrapper：APIKey / ModelService / Endpoint /
                         QuotaPolicy / Subscription；llm_gateway_repo_cache_total
                         metric 上报 hit/miss）
internal/infra             ── DB / Redis / Kafka adapters + schema.sql + Migrate
internal/domain            ── 跨包共享 typed structs（RequestContext / Endpoint / ...）
internal/config            ── gateway.yaml loader

internal/app/gateway  ── 数据面 composition root
internal/builtin      ── 内置 vendor / translator 的唯一注册入口
cmd/gateway           ── 精简的数据面进程入口
cmd/console           ── 可选控制面 Admin API
cmd/migrate           ── 版本化数据库迁移命令
cmd/mockupstream      ── dev/test 假上游
scripts/{e2e-smoke,seed-e2e}                       单 vendor 端到端烟测
scripts/{e2e-smoke-multivendor,seed-multivendor}   多 vendor 端到端烟测（见 testdata/fieldmatrix/endpoints/）
docs/architecture/    设计文档（00-overview 至 08-observability）
configs/              per-environment 配置（local / prod / docker）
```

## 快速开始

生产环境应先运行 `make run-migrate`。local/docker 配置为了开发便利显式开启
`database.auto_migrate`。
业务数据（model_services / endpoints / api_keys / pricing / quota_policies /
subscriptions / accounts）既可以直接通过 SQL 维护，也可以使用可选的
`cmd/console` 控制面；数据面不依赖 console。

```sh
# 1. 通过 Docker 启动本地 stack（MySQL + Redis + Redpanda）。
make stack
# （或者：docker compose up -d）

# 2. 执行迁移并启动 gateway（local 配置也会幂等地自动迁移）。
make run-migrate
make run-gateway
# （或者：go run ./cmd/gateway -config ./configs/local/gateway.yaml）

# 3. 直接用 SQL 插入一条 model_service + endpoint + api_key。
#    示例 seed 见 examples/full-config/seed.sql
mysql -h 127.0.0.1 -uroot llm_gateway < examples/full-config/seed.sql

# 4. 发一个请求。
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-test-alice" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hi!"}]}'
```

### 测试

```sh
make test               # 单元测试；没设 MYSQL_DSN 时 SQL 相关测试会 skip
make test-integration   # 起 stack 后跑全部测试，包含 SQL/outbox
make cover              # 单元测试 + 覆盖率统计（MYSQL_DSN/REDIS_ADDR 的 gating 规则同上）
```

覆盖率是 `go tool cover` 给出的语句覆盖率（不是分支覆盖率），是本次提交时的快照——
只跑了 `make test` 默认档（不设 `MYSQL_DSN`/`REDIS_ADDR`，SQL/Redis 相关测试会被 skip，
在这里算 0%）。`make cover` 只统计 `internal/...` 下自己有测试文件的包
（`cmd/*`/`scripts/*` 本身就是没有测试的薄入口，设计上如此）：

| | |
|---|---|
| **总计** | **57.8%** |
| `internal/dispatch` | 87.3% |
| `internal/invoker` | 89.5% |
| `internal/middleware` | 79.2% |
| `internal/protocol/quirks` | 96.2% |
| `internal/router` | 82.1% |
| `internal/translator/*`（平均） | ~72% |
| `internal/repo`、`internal/infra`、`internal/console` | 5-15%（主要靠 SQL 驱动的测试覆盖，见 `make test-integration`） |

想要当前准确数字，本地跑一下 `make cover`；`go tool cover -html=coverage.out`
能在浏览器里看逐行的覆盖情况。

`gateway.yaml` 控制 server 设置（监听地址、超时、body 大小限制）、数据库连接、
outbox driver 以及各 middleware 的可调参数。默认值是合理的开箱即用值——
完整 schema 见 [`internal/config/config.go`](internal/config/config.go)。

Gateway 默认监听 `:8080`。用仓库自带的配置时：

| Endpoint | Method | 说明 |
|----------|--------|------|
| `/healthz` | GET | 存活探针 |
| `/readyz` | GET | 就绪探针 |
| `/metrics` | GET | Prometheus 抓取端点 |
| `/v1/chat/completions` | POST | OpenAI Chat Completions |
| `/v1/messages` | POST | Anthropic 风格 chat |
| `/v1/embeddings` | POST | OpenAI Embeddings |
| `/v1/images/{generations,edits,variations}` | POST | OpenAI Images |
| `/v1/audio/{speech,transcriptions,translations}` | POST | TTS + ASR |

路由按 modality 定义在 [`internal/router/`](internal/router/) 下——每个 modality 文件
（`chat.go` / `image.go` / `audio.go` / `embedding.go`）注册自己的路径，并显式
列出自己的 middleware 链。

### 配置文件

各环境的配置放在 [`configs/`](configs/) 下（见
[`configs/README.md`](configs/README.md)）。

单个环境目录下只有一个文件：
- `gateway.yaml` —— server / middleware / database / redis / outbox

业务数据存在 MySQL 里，`cmd/migrate` 负责版本化 schema 迁移；增删改查可使用 SQL
或 `cmd/console`。repo 层用进程内 TTL LRU 缓存读操作（默认约 30s）；API Key
撤销还支持 best-effort cachebus 主动失效，将传播时间缩短到秒内。

修改 `gateway.yaml` 需要重启才能生效。

## 构建 / 测试

```sh
go build ./...
go test ./...
```

## 许可证

Apache-2.0 —— 见 [LICENSE](LICENSE)。
