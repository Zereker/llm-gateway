# llm-gateway

[English](README.md) | [简体中文](README.zh-CN.md)

[![codecov](https://codecov.io/gh/Zereker/llm-gateway/graph/badge.svg)](https://codecov.io/gh/Zereker/llm-gateway)

**面向企业 LLM 流量、策略感知、兼容 OpenAI 的运行时网关。**

`llm-gateway` 为平台团队提供统一的运行时治理点，在托管模型与自建模型之上
集中处理认证、路由、配额、内容治理、计量、审计和可观测性。它采用控制面与
数据面分离的设计，而不是一个简单的反向代理。

[快速开始](#快速开始) · [架构文档](docs/architecture/) ·
[演进路线](docs/ROADMAP.md) · [性能基准](examples/benchmark/) ·
[English](README.md)

## 为什么选择 llm-gateway

- **统一客户端契约：** OpenAI、Anthropic 等请求协议进入同一条路由流水线，
  并可转换为不同的上游协议。
- **可靠的模型流量治理：** 能力过滤、配额预占、P2C、冷却、成功率/延迟评分、
  重试以及显式跨模型 fallback。
- **受治理的访问：** 账号订阅、API Key 与账号双层配额、输入/输出 moderation、
  内容日志和用量事件。
- **独立控制面：** 可选 Console 管理模型、Endpoint、密钥、策略、价格和审计，
  但不成为数据面的运行依赖。
- **默认可运维：** Prometheus 指标、结构化 Trace、就绪探针、上游凭证加密，
  以及可插拔 SQL/Redis/Kafka 基础设施。

```text
应用 / SDK
    │  OpenAI / Anthropic 兼容 API
    ▼
┌──────────────────── llm-gateway 数据面 ─────────────────────┐
│ 认证 → Catalog → 配额 → Moderation → Dispatch → 用量 / Trace │
│                                      │                      │
│                          过滤 → 评分 → 重试/fallback         │
└──────────────────────────────────────┼──────────────────────┘
                                       ▼
                  OpenAI · Anthropic · Gemini · Bedrock
                  OpenAI 兼容服务 · vLLM · Ollama

             Console 控制面 ── MySQL ── 数据面缓存
```

## 当前能力

| 领域 | 已实现能力 |
|---|---|
| API | OpenAI Chat/Responses、Anthropic Messages、Embedding、图片、音频 |
| 上游 | OpenAI 兼容服务、Anthropic、Gemini/Vertex、Bedrock、Cohere、Azure OpenAI |
| 路由 | weighted/P2C、并发感知、冷却、动态评分、重试、显式模型 fallback |
| 治理 | API Key 认证、账号订阅、双层配额、moderation chain、内容日志、写操作审计 |
| 运维 | Admin API + Web Console、Prometheus、OTel/slog Trace、用量 Outbox、Helm 示例 |

规则驱动的 Virtual Model、带作用域的 Prompt Policy 和企业身份体系仍属于演进
路线，不作为当前已完成功能宣传。

## 快速开始

使用 Docker 运行完整 Demo。它会启动 MySQL、Redis、Gateway、Web Console、
Mock LLM 上游，并自动完成数据库迁移和幂等初始化：

```sh
make -C examples/demo up
```

命令会通过 Gateway 发起一次真实请求并打印响应。启动完成后：

- Gateway：`http://localhost:8080`
- Console：`http://localhost:8081`，Token 为 `demo-admin-token`
- Demo API Key：`sk-demo-llm-gateway`

```sh
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-demo-llm-gateway" \
  -H "Content-Type: application/json" \
  -d '{"model":"mock-openai-model","messages":[{"role":"user","content":"你好！"}]}'

make -C examples/demo down
```

Demo 使用仓库自带的 Mock 上游和仅供开发的固定凭据，不会调用真实模型供应商。
它的 Compose、Dockerfile、配置和生命周期命令均收口在
[`examples/demo`](examples/demo/) 中。

## 状态

核心数据面、控制面和多供应商协议路径已经实现。架构契约记录在
[`docs/architecture`](docs/architecture/) 中；产品演进与验收标准记录在
[`roadmap`](docs/ROADMAP.md) 中。

## 数据面内部结构

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

## 本地开发流程

当你需要在宿主机运行 Go 进程参与开发时使用这套流程，而不是完整 Demo。
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

上面的徽章是 CI 实时跑出来、按文件统计的覆盖率（见
[`.github/workflows/ci.yml`](.github/workflows/ci.yml) 的 `go` job，上传到
[codecov.io/gh/Zereker/llm-gateway](https://codecov.io/gh/Zereker/llm-gateway)）——
那个 job 起了 MySQL/Redis/Kafka，所以也覆盖了 `make cover` 本地默认跳过的
SQL/Redis 相关测试。想在推送前先看一下本地数字，跑 `make cover`（只测单元测试，
不设 `MYSQL_DSN`/`REDIS_ADDR`，只统计 `internal/...` 下自己有测试文件的包）；
`go tool cover -html=coverage.txt` 能在浏览器里看逐行的覆盖情况。

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
