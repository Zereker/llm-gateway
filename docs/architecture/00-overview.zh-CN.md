[English](00-overview.md) | [简体中文](00-overview.zh-CN.md)

# 00 — 概述

## 1. 项目目标

`llm-gateway`是LLM请求网关，提供统一的客户端入口点，根据主账户、模型、组和端点配置将请求转发到上游供应商或自托管模型服务。

目标架构重点解决：

1. **多协议入口和上游协议转换**：OpenAI Chat / Responses、Anthropic Messages 和其他入口点都流入同一路由管道，然后由转换器转换为上游的本机协议。
2. **主账户控制**：API key解析为主账户的pin码、子账户/运营商、群组和配额策略；模型可见性由订阅表控制。
3. **端点选择和重试**：候选者由模型+组拉出，首先过滤协议/模式资格，然后在同一模型内交换端点；跨模型回退仅在通过标头显式声明时执行。
4. **Redis限速**：两层配额策略（主账户+API key）；请求前保留RPM/RPS，请求后根据实际用量扣除TPM；仅针对最终选择的端点扣除端点配额。
5. **日志记录和计量输出**：内容日志记录、使用事件和指标/跟踪是三个独立的通道；网关仅产生事实数据——定价是在下游完成的。
6. **可观察性**：slog 跟踪字段、Prometheus 指标、可选的 OpenTelemetry 跟踪器。

## 2. 非目标

- 本身不实现模型推理服务。
- 不实现 RAG、提示编排、代理或业务 BFF 逻辑。
- 业务表由部署者直接通过SQL维护；还可以使用单独的 `cmd/console` 控制平面二进制文件（管理 API）来管理它们，但数据平面从不依赖于它。
- 不在网关进程内进行计费聚合；网关仅发出使用事件，定价聚合由下游作业完成。

<a id="3-running-processes"></a>
## 3. 运行进程

|流程|切入点|责任|
|------|------|------|
|网关| `cmd/gateway` |精简数据平面入口；应用schema migration，然后在 `internal/app/gateway` 中组装运行时 |
|控制台| `cmd/console` |控制平面：用于管理业务数据的管理API（由`internal/console`支持）；可选，数据平面无需它即可运行 |

业务数据（accounts/api_keys/model_services/endpoints/quota_policies/
策略定义/策略绑定/
订阅/定价版本）由部署者通过 SQL INSERT/UPDATE/DELETE 直接维护；
单独的 `cmd/console` 控制平面二进制文件是管理它的另一种方法（它也可以发布
网关订阅的缓存总线失效以实现快速撤销）。

网关启动顺序：

1. 加载选定的`gateway.yaml`。
2. 初始化端点验证加密数据密钥。
3. 打开 **SQL DB（必需）**，应用挂起的版本化迁移，然后运行 `repo.CheckSchema`；
   迁移和架构验证共享 30 秒的启动截止时间。
4.打开**Redis（必填）**； M6 速率限制和调度程序冷却都依赖于 Redis。
5. 组装 SQL 读取器/提供程序（包装在 `repo.CachedXxxReader` TTL LRU 层中，请参阅
   [06§8](./06-pluggable-infra.zh-CN.md#8-repo-cache-deployer-sql--gateway-data-propagation)),
   Redis 速率限制存储、调度程序、Outbox、跟踪器。
6. 扫描启用的端点，验证供应商适配器是否存在，`endpoint.Protocol` 是否有效，并且所需的转换器已注册；如果丢失，则仅记录警告并发出指标，而不会阻止启动。
7、拨打`router.NewEngine`注册路由和中间件。
8. 交给 `internal/app/runtime` 进行监听、信号处理、正常关闭和逆序资源拆卸。

部署顺序：

1. docker 堆栈启动 MySQL + Redis（如果使用 kafka Outbox驱动程序，则启动 Kafka/Redpanda）。
2.启动网关；每个副本在准备就绪之前都会运行幂等迁移例程。
3.通过SQL或可选控制台管理业务数据。
4. 部署者 SQL-INSERT 业务数据，例如账户、API 密钥、模型服务、订阅、端点、
   和配额策略（endpoint.auth 列必须使用 `repo.EncodePayload` 进行加密，
   并且 api_keys.api_key_hash 列必须使用 `repo.HashAPIKey` 作为 SHA-256 十六进制进行计算。

SQL→网关数据传播经过**repo层的进程内TTL LRU缓存**（默认30秒）：部署者写入
MySQL → 一旦网关存储库缓存自然过期，未命中就会触发直接 SQL 查找以获取新值。 **没有直接的每个请求
针对同一数据库的查询**。大多数记录依赖于 TTL 过期时间。 API 密钥撤销是故意的
例外：当启用控制台缓存总线时，它会发布尽力而为的目标失效； TTL 仍然是回退方案。参见
[06 §8](./06-pluggable-infra.zh-CN.md#8-repo-cache-deployer-sql--gateway-data-propagation) 了解详细信息。

架构更改通过 `internal/infra` 中的版本化迁移进行，并在网关启动期间应用。
更改必须保持向后兼容：首先部署具有新模式的新网关，让它创建新表/列（保留旧字段）；
仅当所有网关完成升级后，您才可以删除字段或进行重大更改。

## 4. 请求生命周期

```text
HTTP request
  |
  | pre: BodyLimit / Timeout
  v
M1 TraceContext      Generates RequestID, injects OTel SpanContext/Baggage, creates requeststate.State
M9 Recover           defer-based fallback for panics and unified error responses
M2 Auth              Parses API key/JWT into domain.UserIdentity
M3 Envelope          Reads the raw body, extracts model, records source protocol + modality
M4 Budget            alwayspass or inmemory gate; failure aborts immediately
M5 ModelService      Looks up the global model catalog and the primary account's subscription
M6 Limit             Pre-deducts the user-side RPM/RPS, post-deducts TPM based on usage after the response
M8 Moderation        Optional content moderation; defaults to none
M7 Schedule          Pulls endpoint candidates, schedules, retries, forwards upstream, writes Usage/Decision/Error
M10 Tracing          metric, usage meta, outbox, scheduling trace
  |
  v
HTTP response
```

注：M6使用gin的洋葱模型，在`c.Next()`之后执行用户端TPM `ChargeBatch`； M10 中没有这样做。

## 5. 组件分层

```text
cmd/gateway
  -> config.Load
  -> runtime.OpenDB/OpenRedis/NewKafkaProducer
  -> repo SQL readers/providers
  -> router.NewEngine

internal/router
  -> Registers the full /v1/... routes per modality
  -> Composes the middleware chain

internal/middleware
  -> Request lifecycle, RequestContext read/write, error abort
  -> Each middleware has its own custom Option (interface) + WithXxxTracerProvider, aligned with otelgin v0.68.0

internal/repo (cached)
  -> SQL Reader/Provider wrapped in a TTL LRU layer (repo.TTLCache[K,V] + CachedXxx wrapper)
  -> ModelCatalog / EndpointReader / APIKeyProvider and other middleware-owned readers use the cached version directly

internal/selector
  -> Performs filter / pick / report over a batch of candidate endpoints; holds no repo, does not switch fallback model

internal/invoker
  -> Handler lookup, HTTP Do, response forward

internal/protocol
  -> Handler facade (PrepareCall → NewResponseStream)
  -> Factory / Session: vendor HTTP layer (URL, auth header, request construction)
  -> protocol/quirks: endpoint-level body/header tweak DSL (rename / strip / set / set_default)

internal/translator
  -> Request/response conversion between the client protocol and the upstream protocol, usage extraction

internal/repo + internal/infra
  -> SQL schema, CRUD/readers, Redis, Kafka
```

## 6. 关键术语

|术语 |预期含义|
|------|----------|
| pin | 账户的稳定外部标识符，用作计费主体 ID；与数据库自增主键不同，由部署者在创建账户时分配，此后不可变 |
| group | 主账户下的请求分组维度，影响端点候选过滤；默认为 `default`，可用于 `reserved` / `experimental` 等隔离场景 |
| `requeststate.State` | HTTP管道状态附加到`c.Request.Context()`；故意不是域实体 |
| `RequestEnvelope` | M3的输出：原始主体、模型、源协议、模态；不再包含规范请求 |
| `UserIdentity` | M2 的输出，包含主账户的 PIN、子账户/运营商、API 密钥、组和配额策略 ID |
| `ModelService` |全球车型目录记录；主账户是否可以使用由订阅表决定 |
| `Endpoint` |全球上游接入点，型号+组匹配；包括供应商、权重、身份验证、路由、配额、功能 |
| `Adapter` |供应商的HTTP层工厂/会话；不负责协议转换或使用聚合 |
| `Translator` |协议转换层；负责请求正文转换、响应处理器、用量提取 |
| `Scheduler` | M7 的批内端点选择器，公开 Pick/Report，不负责跨模型回退 |
| `Usage` | M10 发布到Outbox的单个请求的资源消耗和元数据 |
| `TTLCache` |网关的进程内LRU + TTL缓存（`internal/repo/cache.go`），repo的唯一缓存策略 |
| `CachedXxxReader` | repo层的缓存包装器，用TTL LRU层包装SQL Reader；默认30秒|

## 7. 文档版本

|版本 |日期 |笔记|
|------|------|------|
| v0.3-目标 | 2026-05-16 |对齐的目标边界：协议功能下推到端点、简化的调度程序、RPM/RPS 预扣、TPM 后扣、下游计费 |
| v0.4-目标 | 2026-05-17 |与 otelgin v0.68.0 一致的中间件选项；域/存储库完全解耦 |
| v0.6-目标 | 2026-05-21 |删除 `cmd/admin` + Flink + Debezium CDC：数据平面是 100% 只读 MySQL，repo 使用 TTL LRU 缓存而不是实时失效 |
| v0.7-目标 | 2026-05-22 |将原来的 `pkg/adapter` 合并到协议包中（vendor Factory / Session / Classifier 都位于协议包中）； endpoint.quirks JSON 列 + DSL；调度/存储库缓存连接到 OTel 和 Prom |
