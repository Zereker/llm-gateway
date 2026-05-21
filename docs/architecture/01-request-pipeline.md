# 01 — Request Pipeline

本文记录目标 `pkg/router` + `pkg/middleware` 请求链路，以及 `domain.RequestContext` 的目标字段。

## 1. 路由装配

`pkg/router.NewEngine` 创建 `gin.Engine` 并注册：

- ops 路由：`/healthz`、`/readyz`、`/metrics` 等在 `helpers.go` 中维护。
- chat：`/v1/chat/completions`、`/v1/messages`、`/v1/responses`。
- image：`/v1/images/{generations,edits,variations}`。
- audio：`/v1/audio/{speech,transcriptions,translations}`。
- embedding：`/v1/embeddings`。

路由文件自己声明完整 `/v1/...` 路径，不使用全局 `/v1` group。各模态文件显式列出自己的 middleware 链，避免共享 helper 把不同模态绑死。

## 2. RequestContext 存储方式

`RequestContext` 通过 `context.WithValue` 挂在 `c.Request.Context()` 上，而不是 `gin.Context.Set/Get`。

入口 helper：

- `middleware.AttachRequestContext(c, rc)`：仅 M1 调用。
- `middleware.GetRequestContext(c)`：取不到则 panic，由 M9 Recover 兜底。

这样 RequestContext、OTel SpanContext、Baggage 都在同一个 stdlib context 容器中传递。

## 3. `domain.RequestContext`

目标定义：

```go
type RequestContext struct {
    RequestID string
    StartTime time.Time

    Identity UserIdentity
    Envelope *RequestEnvelope

    ModelService *ModelService // 原始请求 model
    ModelChain   []*ModelService // M5 预解析的尝试序列：primary + 已校验过的 fallback
    RoutedModelService *ModelService // 实际成功 model，未 fallback 时等于 ModelService

    RateLimit *RateLimitState

    Endpoint *Endpoint

    Usage              *Usage
    Error              *AdapterError
    SchedulingDecision *SchedulingDecision

    Extras map[string]any
}
```

重要约束：

- `trace_id` / `span_id` 不作为字段保存；从 `c.Request.Context()` 中的 OTel context 提取（`middleware.TraceIDFromCtx`）。
- **`context.Context` 不挂在 RC 上**——单源真相是 `c.Request.Context()`。Middleware 拿 ctx 走 `c.Request.Context()`，回写走 `c.Request = c.Request.WithContext(ctx)`。把 ctx 字段挂在 mutable struct 上违反 Go 「context is values, not state」原则，还会跟 gin 原生 `c.Request.Context()` drift。
- 不保存 `*gin.Context`；响应写出由 middleware 使用当前 handler 的 `c.Writer`。
- 不保存 `*slog.Logger`；日志使用 `slog.*Context`，`trace.CtxHandler` 自动补 trace/baggage 字段。
- 业务代码必须使用 `slog.InfoContext` / `ErrorContext` / `WarnContext` 等带 context 的方法；禁止在请求路径直接调用 `slog.Info` / `Error`，否则 trace 字段无法注入。
- M4 Budget 不写 `BudgetStatus` 到 RC；通过即继续，失败即 abort。
- adapter session 不挂 RC；只保留响应阶段产物 `Usage`、`Error`、`SchedulingDecision`。

## 4. Middleware 链

| 编号 | 名称 | 主要输入 | 主要输出/副作用 |
|------|------|----------|-----------------|
| pre | BodyLimit | config `middleware.body_limit_bytes` | 超限返回 413 |
| pre | Timeout | config `middleware.timeout` | 给请求 context 设置超时 |
| M1 | TraceContext | HTTP headers/context | 创建 RC、RequestID、span/baggage |
| M9 | Recover | RC/Error/panic | 统一错误 JSON、panic 兜底 |
| M2 | Auth | Authorization / X-API-Key | `rc.Identity` |
| M3 | Envelope | request body + 路由协议标记 | `rc.Envelope`，body 可重复读取 |
| M4 | Budget | `rc.Identity` | gate 失败 abort |
| M5 | ModelService | `rc.Envelope.Model`、主账号 pin、`X-Gateway-Fallback-Models` | `rc.ModelService`（primary）、`rc.ModelChain`（primary + 已校验 fallback） |
| M8 | Moderation | raw request/response stream | 可选审核，默认 none |
| M6 | Limit | identity、model、quota policy | 用户侧 RPM/RPS 前扣、`rc.RateLimit`；post-side TPM 后扣 |
| M7 | Schedule | model、group、endpoint candidates | `rc.RoutedModelService`、endpoint、upstream forward、usage、decision |
| M10 | Tracing | RC 终态 | metric、usage outbox、schedule trace |

M9 注册在早期，但通过 defer 覆盖 M2 之后所有 middleware 和 handler 的 panic；pre middleware（BodyLimit / Timeout）必须自身不可 panic 或自行兜底。M10 在链尾，通过 `c.Next()` 后执行收尾逻辑。

## 5. M2 Auth

目标依赖由 M2 自己声明最小接口，repo 只作为实现。API key 存储为 SHA-256 hash，解析后得到 `domain.UserIdentity`：

```go
type UserIdentity struct {
    AccountID            string // 主账号 pin / 计费主体
    SubAccountID         string // 子账户 / 操作者
    APIKeyID             string
    Group                string
    ExternalUser         bool
    AccountQuotaPolicyID *int64 // 主账号级限流策略
    APIKeyQuotaPolicyID *int64 // API key 级限流策略
}
```

`AccountID` 是主账号 pin / 计费主体；`SubAccountID` 是该主账号下的子账户 / 操作者。`AccountQuotaPolicyID` 来自 `accounts` join，`APIKeyQuotaPolicyID` 来自 `api_keys`。M6 用这两个 ID 避免每请求重复查主账号和 key 的策略关系。

API key 查询基于 `key_hash` 唯一索引；hash 命中后从 accounts / api_keys join 出 `AccountID`、`SubAccountID` 和策略 ID，调用方不能通过 header 改变归属。`SubAccountID` 由 API key 记录关联，不从请求 header 直接信任。理论 hash 冲突视为系统错误，返回 500/503 并告警。

M2 解析完凭证后，后续日志、trace baggage、上游转发和 Content Log 都不得携带 `Authorization` / `X-API-Key` 原值；需要透传上游认证时只使用 endpoint auth 配置重新生成 header。

## 6. M3 Envelope

`RequestEnvelope` 只承载路由需要的最小信息：

```go
type RequestEnvelope struct {
    RawBytes       []byte
    Model          string
    SourceProtocol Protocol
    Modality       Modality
}
```

M3 不做 canonical 化，不解析完整协议字段。请求体 shape 转换交给 `pkg/translator`。

## 7. M5 ModelService

M5 使用 middleware-owned interfaces，由 `pkg/repo` 的 cached wrapper 注入：

- `ModelCatalog.GetByModel` 查全局 model catalog（生产实现：`repo.CachedModelServiceReader`
  TTL LRU 命中直接返回；miss 走 `repo.SQLModelServiceReader.GetByModel`；TTL 默认 30s）。
- `SubscriptionChecker.HasModel` 校验主账号是否订阅该 model（同样用 cached wrapper）。

`model_services` 是全局 catalog，不再按主账号存储；主账号可见性由 `account_model_subscriptions` 表决定。

5 个 cached wrapper 的清单 + 默认参数见 [06 §8.2](./06-pluggable-infra.md#82-适用表与默认参数)。

M5 不查询 active pricing，也不向 `RequestContext` 写 pricing snapshot。价格匹配和金额计算由下游计费平台根据 Usage Event 的请求发生时间完成。

跨 model fallback 校验也在 M5 完成：解析 `X-Gateway-Fallback-Models` header（详见 [03 §5](./03-endpoint-scheduling.md#5-重试模型)），对每个 fallback model 重新走 catalog + subscription，把已校验过的 `*ModelService` 一并写入 `rc.ModelChain`（primary 在 `[0]`，fallback 顺序追加）。fallback 不存在或未订阅时静默剔除——只要 primary 成功，请求继续。M7 直接遍历 `rc.ModelChain`，不再重做 catalog/subscription。成功选中某个 model 后 M7 写 `rc.RoutedModelService`，usage meta 和 attempt 记录都使用实际路由 model。

SQL 查询失败统一按依赖故障处理：M2 IdentityResolver、M5 ModelCatalog、SubscriptionChecker、M7 EndpointReader 返回 DB error 时 fail-closed，响应 503 和 `Retry-After`，不得伪装成 401/403/404。PolicyCache 的显式 TTL 缓存是例外；缓存命中时可继续使用缓存值，cache miss 后 DB 失败仍返回 503。

## 8. 错误出口

早期 middleware 通过内部 `abort(c, status, class, message)` 写 `rc.Error` 并 `c.Abort()`。M9 Recover 统一把 `rc.Error` 转成响应。

M7 响应流已经开始后，后续错误不能再覆盖 HTTP 状态；此时只写 `rc.Error` 供日志、metric 和 outbox 参考。

统一错误响应 body 使用顶层 `error` 包装，便于未来扩展非错误返回字段：

```go
type ErrorResponse struct {
    Error ErrorBody `json:"error"`
}

type ErrorBody struct {
    Code      string         `json:"code"`
    Message   string         `json:"message"`
    Class     string         `json:"class"`
    Details   map[string]any `json:"details,omitempty"`
    RequestID string         `json:"request_id"`
    TraceID   string         `json:"trace_id"`
}
```

约束：

- `Code` 是稳定机器可读错误码，例如 `rate_limit_exceeded`、`invalid_request`、`no_endpoint_available`。
- `Message` 是人类可读描述，不作为程序判断依据。
- `Class` 对齐调度/上游错误分类，例如 `invalid`、`capacity`、`transient`。
- `Details` 只放排障必要字段，例如限流维度、bucket key、endpoint id；不得放 request / response body。
- `RequestID` / `TraceID` 由 M9 Recover 从 `rc` 中填充；客户端可据此与日志 / trace 关联，无需另开 header。

JSON 示例参见 [08-observability §7](./08-observability.md#7-error-response)。

## 9. Framework Boundary

文档中出现的 `c.Next()` 表示 gin onion model 的 post-handler phase；目标契约是“前置处理 -> 下游 handler/middleware -> 后置收尾”，不是要求业务逻辑依赖 gin API。若未来替换 HTTP 框架，必须保留这个执行语义。

## 10. Middleware 装配契约（otelgin v0.68.0 对齐）

每个 middleware 暴露：

- `XxxOption` interface + 私有 `xxxOptionFunc` adapter（不再用 function-type
  `func(*cfg)`）。
- 必需依赖的 `WithXxxFoo(...)` constructor，缺则构造期 panic。
- 可选依赖的 `WithXxxBar(...)`，nil 时给明确默认（如 moderator nil = pass-through）。
- `WithXxxTracerProvider(tp oteltrace.TracerProvider)`：注入 OTel TracerProvider；
  nil 时启动期退到 `otel.GetTracerProvider()`。

middleware 启动期一次性：

```go
cfg := xxxConfig{}
for _, opt := range opts { opt.apply(&cfg) }
if cfg.required == nil { panic("middleware.Xxx: WithXxxRequired required") }
if cfg.tracerProvider == nil { cfg.tracerProvider = otel.GetTracerProvider() }
tracer := cfg.tracerProvider.Tracer(ScopeName)   // 闭包持有
return func(c *gin.Context) { /* 热路径 */ }
```

热路径标准模板（ctx 接力的单源真相是 `c.Request.Context()`，**不**走 RC 字段）：

```go
return func(c *gin.Context) {
    ctx, span := tracer.Start(c.Request.Context(), "xxx.action")
    defer span.End()
    c.Request = c.Request.WithContext(ctx)   // ← 下游 mw 自动接力 span

    rc := GetRequestContext(c)               // RC 只承载数据，不带 ctx
    // ... 业务调用全部传局部 ctx：cfg.dep.Call(ctx, ...)
}
```

Pass-through 快路径（pass-through moderator / no budget gate / no ratelimit
store）在 startup 期就 return `func(c) { c.Next() }`，连 tracer 都不开。详见
[06 §6 Middleware Options](./06-pluggable-infra.md#6-middleware-options)。

M1 TraceContext 是参考实现，额外暴露 `WithTraceContextPropagators` /
`WithSpanNameFormatter` / `WithTraceContextSpanStartOptions`，完整对位 otelgin 的
`WithPropagators` / `WithSpanNameFormatter` / `WithSpanStartOptions`。

## 11. 演进规则

- 新增 RC 字段时，必须标明写入 middleware 和读取方。跨 model fallback 相关字段必须区分原始请求 model 与实际路由 model。
- 新增 middleware 时，更新本文件的顺序表，并检查所有模态路由是否需要接入。
- 请求路径日志应增加 lint/test 约束，扫描 `slog.Info(`、`slog.Error(`、`slog.Warn(` 等非 Context 调用。
- 不要把临时实验字段直接加入 RC；先放 `Extras`，稳定后再升级为正式字段。
