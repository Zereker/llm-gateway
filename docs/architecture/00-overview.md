# 00 — Overview

## 1. 项目目标

`llm-gateway` 是 LLM 请求网关，提供统一客户端入口，按主账号、模型、分组和 endpoint 配置把请求转发到上游厂商或自部署模型服务。

目标架构重点解决：

1. **多协议入口与上游协议转换**：OpenAI Chat / Responses、Anthropic Messages 等入口统一进入路由链路，再由 translator 转成上游原生协议。
2. **主账号控制**：API key 解析出主账号 pin、子账户/操作者、group 和 quota policy，模型可见性由订阅表控制。
3. **endpoint 选择与重试**：按 model + group 拉候选，先做协议/模态资格过滤，再在同 model 内换 endpoint；跨 model fallback 只按 header 显式声明执行。
4. **Redis 限流**：主账号 + API key 双层 quota policy，RPM/RPS 请求前 reserve，TPM 按真实 usage 后扣；endpoint quota 只对最终选中的 endpoint 扣减。
5. **记录与计量输出**：内容记录、Usage Event、Metrics / Trace 三条通道独立；网关只产生事实数据，计价由下游完成。
6. **可观测性**：slog trace 字段、Prometheus metrics、可选 OpenTelemetry tracer。

## 2. 非目标

- 不实现模型推理服务本身。
- 不实现 RAG、prompt 编排、agent 或业务 BFF 逻辑。
- 不做管理 UI；无独立管理面 binary——业务表由 deployer 直接 SQL 维护。
- 不在 gateway 进程内做账单聚合；gateway 只产出 usage 事件，计价聚合由下游任务完成。

## 3. 运行进程

| 进程 | 入口 | 职责 |
|------|------|------|
| gateway | `cmd/gateway` | 数据面：鉴权、解析、限流、调度、上游转发、usage outbox；启动期自跑 `infra.Migrate` 建表 |
| Debezium Server | 独立容器 `debezium/server` | 拉 MySQL binlog → 推 Redis Stream，SQL → gateway 数据传播桥（见 [06 §8 CDC](./06-pluggable-infra.md#8-cdcsql--gateway-数据传播)） |

业务数据（accounts / api_keys / model_services / endpoints / quota_policies /
subscriptions / pricing_versions）由 deployer 直接 SQL INSERT/UPDATE/DELETE 维护——
本项目不带控制面 binary。

gateway 启动流程：

1. 加载 `configs/*/gateway.yaml`。
2. 初始化 endpoint auth 加密 data key。
3. 打开 **SQL DB（必需）**，执行 `infra.Migrate` 建表（`schema.sql` 全 `IF NOT EXISTS`，
   幂等）+ `repo.CheckSchema` 防御性校验；缺表直接退出。
4. 打开 **Redis（必需）**；M6 限流、scheduler cooldown、CDC stream 消费都依赖 Redis。
5. 装配 SQL reader/provider、Redis rate limit store、scheduler、outbox、tracer、
   `cdc.TieredCache` + `cdc.StreamConsumer`（订阅 `llm_gateway.llm_gateway.<table>`
   Stream，详见 [06 §8](./06-pluggable-infra.md#8-cdcsql--gateway-数据传播)）。
6. 扫描 enabled endpoints，校验 vendor adapter 存在、`endpoint.Protocol` 合法、必要 translator 已注册；缺失只记 warn 和 metric，不阻塞启动。
7. 调用 `router.NewEngine` 注册路由和 middleware。
8. 交给 `pkg/server` 处理 listen、signal、graceful shutdown 和资源倒序关闭。

部署顺序：

1. docker stack 起 MySQL（binlog `ROW` + GTID） + Redis + Debezium Server。
2. 启动 gateway——`infra.Migrate` 自动建表。
3. deployer SQL INSERT 录入账号、API key、model service、subscription、endpoint、
   quota policy 等业务数据（endpoint.auth 列要用 `repo.EncodePayload` 加密，
   api_keys.api_key_hash 列要用 `repo.HashAPIKey` 算 SHA-256 hex）。
4. CDC consumer 启动从 `$` 起读（不持久化 stream offset；
   见 [06 §8.2](./06-pluggable-infra.md#82-位点策略)）。

SQL → gateway 数据传播走 Debezium binlog CDC：deployer 写 MySQL → Debezium 捕获 →
Redis Stream → gateway `TieredCache.HandleEvent` 失效 L1。**不直连同库每请求查表**，
也**不**用 outbox 表方案。L1 cold start / Debezium 不可达时降级到 L3 直查 MySQL。

schema 变更走 `pkg/infra/schema.sql`，gateway 启动期跑 `infra.Migrate` 应用。
变更必须保持向后兼容：先部署带新 schema 的新 gateway，让它建好新表/新列（旧字段保留）；
全部 gateway 升级完成后再执行删除字段或破坏性变更。

## 4. 请求生命周期

```text
HTTP request
  |
  | pre: BodyLimit / Timeout
  v
M1 TraceContext      生成 RequestID，注入 OTel SpanContext/Baggage，创建 RequestContext
M9 Recover           defer 兜底 panic 和统一错误响应
M2 Auth              API key/JWT 解析为 domain.UserIdentity
M3 Envelope          读取原始 body，提取 model，记录 source protocol + modality
M4 Budget            alwayspass 或 inmemory gate；失败直接 abort
M5 ModelService      查全局 model catalog、主账号订阅
M8 Moderation        可选内容审核；默认 none
M6 Limit             用户侧 RPM/RPS 前扣，响应后按 usage 后扣 TPM
M7 Schedule          拉 endpoint 候选，调度、重试、上游转发，写 Usage/Decision/Error
M10 Tracing          metric、usage meta、outbox、scheduling trace
  |
  v
HTTP response
```

注意：M6 使用 gin 洋葱模型，在 `c.Next()` 后执行用户侧 TPM `ChargeBatch`；这件事不在 M10 里做。

## 5. 组件分层

```text
cmd/gateway
  -> config.Load
  -> server.OpenDB/OpenRedis/NewKafkaProducer
  -> repo SQL readers/providers
  -> router.NewEngine

pkg/router
  -> 按模态注册完整 /v1/... 路由
  -> 组合 middleware 链

pkg/middleware
  -> 请求生命周期、RequestContext 读写、错误 abort
  -> 每个 middleware 自定义 Option (interface) + WithXxxTracerProvider，对齐 otelgin v0.68.0

pkg/cdc
  -> Debezium binlog event 解析 + Redis Stream XREAD + TieredCache[T]（L1 LRU + L3 loader）
  -> ModelCatalog 等 middleware-owned reader 通过 TieredCache 适配

pkg/selector
  -> 对一批候选 endpoint 做 filter / pick / report；不持有 repo，不切 fallback model

pkg/invoker
  -> adapter lookup、translator lookup、HTTP Do、响应 forward

pkg/adapter
  -> 厂商 HTTP 层：URL、认证 header、request 构造

pkg/translator
  -> 客户端协议与上游协议的请求/响应转换，usage 提取

pkg/repo + pkg/infra
  -> SQL schema、CRUD/readers、Redis、Kafka
```

## 6. 关键术语

| 术语 | 目标含义 |
|------|----------|
| pin | account 的稳定外部标识符，作为计费主体 ID；不同于数据库自增主键，deployer 创建 account 时分配，创建后不可变 |
| Group | 主账号下的请求分组维度，影响 endpoint 候选过滤；默认 `default`，可用于 `reserved` / `experimental` 等隔离场景 |
| `RequestContext` | 一次请求的状态对象，挂在 `c.Request.Context()`，通过 middleware helper 获取 |
| `RequestEnvelope` | M3 产物：raw body、model、source protocol、modality；不再包含 canonical request |
| `UserIdentity` | M2 产物，包含主账号 pin、子账户/操作者、API key、group、quota policy IDs |
| `ModelService` | 全局模型 catalog 记录；主账号是否可用由 subscription 表决定 |
| `Endpoint` | 全局上游接入点，按 model + group 匹配；包含 vendor、weight、auth、routing、quota、capabilities |
| `Adapter` | 厂商 HTTP 层 factory/session；不负责协议转换和 usage 聚合 |
| `Translator` | 协议转换层；负责 request body 转换、response handler、usage 提取 |
| `Scheduler` | M7 的批内 endpoint 选择器，暴露 Pick/Report，不负责跨 model fallback |
| `RateLimitState` | M6/M7 写入的限流状态，供 TPM 后扣和排障使用；不作为客户端 header 契约 |
| `Usage` | 单次请求资源消耗和 meta，M10 发布到 outbox |
| `TieredCache` | gateway 端本地缓存，L1 LRU + L3 SQL loader；由 CDC event 触发失效 |
| `CDC` | Change Data Capture；Debezium 读 MySQL binlog → Redis Stream → `pkg/cdc` 消费 |
| `Debezium Stream Key` | Redis Stream 命名 `llm_gateway.llm_gateway.<table>`（Debezium 默认 `<db_server>.<schema>.<table>`） |

## 7. 文档版本

| 版本 | 日期 | 说明 |
|------|------|------|
| v0.3-target | 2026-05-16 | 对齐目标边界：协议能力下沉 endpoint、简化 scheduler、RPM/RPS 前扣、TPM 后扣、下游计费 |
| v0.4-target | 2026-05-17 | CDC 数据传播（Debezium binlog → Redis Stream → TieredCache）；middleware Option 对齐 otelgin v0.68.0；domain/repo 彻底解耦 |
