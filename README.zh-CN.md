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
pkg/middleware        ── 请求生命周期（M1-M10）：auth / envelope / budget /
                         catalog / ratelimit / moderation / **M7 → dispatch** /
                         tracing。每个 middleware 单一职责，配 OTel option。
  │
  ▼  (M7 是 dispatch.Dispatcher 的 thin adapter)
pkg/dispatch          ── 调度执行时序的**唯一**所有者：
                         CandidateSource → filterEligible → Selector.Pick →
                         EndpointQuota.Reserve → Handler lookup → Invoker →
                         Selector.Report → RetryPolicy → Stream / Charge
                         （pkg/dispatch/adapters/ 桥接 selector / invoker /
                         ratelimit / repo primitives → dispatch ports）
  │
  ├── pkg/selector    ── primitives：filters / scorer / picker / cooldown。
  │                      只做选择算法，不知道 protocol / handler / middleware
  ├── pkg/invoker     ── primitives：一次 HTTP 调用 + 响应 forward，不做调度
  ├── pkg/ratelimit   ── primitives：Store / Bucket / endpoint bucket helpers
  └── pkg/protocol    ── facade：Handler = Factory + Translator + Quirks
                         消费侧只看 Handler / Lookup；Factory / Session 是内部
       ├── protocol/<vendor>/  OpenAI(+ark alias) / Anthropic / Gemini Factory + Session
       ├── protocol/quirks/    endpoint 级 body+header 微调 DSL（rename/strip/set/set_default）
       └── translator (pkg/)   body shape 转换：identity + 跨协议 pairs（init() 注册）

pkg/moderation        ── Moderator + 响应流装饰器 + ctx helpers
pkg/usage             ── usage 提取 + outbox（file | kafka）+ pricing
pkg/trace / metric    ── Tracer 抽象（slog / OTel）+ Prom metric name 常量

pkg/repo              ── data access：sqlx Reader/Provider + TTL LRU cache wrapper
                         （5 个 cached wrapper：APIKey / ModelService / Endpoint /
                         QuotaPolicy / Subscription；llm_gateway_repo_cache_total
                         metric 上报 hit/miss）
pkg/infra             ── DB / Redis / Kafka adapters + schema.sql + Migrate
pkg/domain            ── 跨包共享 typed structs（RequestContext / Endpoint / ...）
pkg/config            ── gateway.yaml loader

cmd/gateway           ── composition root：buildEngine 装配所有 deps，无业务逻辑
cmd/mockupstream      ── dev/test 假上游
scripts/{e2e-smoke,seed-e2e}  端到端烟测
docs/architecture/    设计文档（00-overview 至 08-observability）
configs/              per-environment 配置（local / prod / docker）
```

## 快速开始

Gateway 是单一 binary，启动时会跑 `infra.Migrate` 建表。
业务数据（model_services / endpoints / api_keys / pricing / quota_policies /
subscriptions / accounts）通过直接向 MySQL 插入 SQL 来维护——
本仓库不提供控制面 / 管理用的 REST API。

```sh
# 1. 通过 Docker 启动本地 stack（MySQL + Redis + Redpanda）。
make stack
# （或者：docker compose up -d）

# 2. 启动 gateway —— 启动期自动跑 infra.Migrate 建表。
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
```

`gateway.yaml` 控制 server 设置（监听地址、超时、body 大小限制）、数据库连接、
outbox driver 以及各 middleware 的可调参数。默认值是合理的开箱即用值——
完整 schema 见 [`pkg/config/config.go`](pkg/config/config.go)。

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

路由按 modality 定义在 [`pkg/router/`](pkg/router/) 下——每个 modality 文件
（`chat.go` / `image.go` / `audio.go` / `embedding.go`）注册自己的路径，并显式
列出自己的 middleware 链。

### 配置文件

各环境的配置放在 [`configs/`](configs/) 下（见
[`configs/README.md`](configs/README.md)）。

单个环境目录下只有一个文件：
- `gateway.yaml` —— server / middleware / database / redis / outbox

业务数据存在 MySQL 里。Gateway 启动期跑 `infra.Migrate` 建表；增删改查通过直接
插入 SQL 完成。repo 层用进程内 TTL LRU 缓存读操作（默认约 30s），因此更新会在
TTL 窗口内自然生效，不需要额外的失效通道。

修改 `gateway.yaml` 需要重启才能生效。

## 构建 / 测试

```sh
go build ./...
go test ./...
```

## 许可证

Apache-2.0 —— 见 [LICENSE](LICENSE)。
