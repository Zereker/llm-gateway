[English](01-request-pipeline.md) | [简体中文](01-request-pipeline.zh-CN.md)

# 01 — 请求管道

该文档记录了 `internal/router` + `internal/middleware` 的请求链，以及 `internal/requeststate.State`。

## 1. 路由组装

`internal/router.NewEngine` 创建 `gin.Engine` 并注册：

- 运营路由：`/healthz`、`/readyz`、`/metrics`等，维护于`helpers.go`。
- 聊天：`/v1/chat/completions`、`/v1/messages`、`/v1/responses`。
- 图片：`/v1/images/{generations,edits,variations}`。
- 音频：`/v1/audio/{speech,transcriptions,translations}`。
- 嵌入：`/v1/embeddings`。

每个路由文件声明自己完整的`/v1/...`路径；未使用全局 `/v1` 组。共享安全/配额/可观察性中间件订单位于 `internal/router/pipeline.go` (`llmRouteGroup` + `registerLLMRoute`)；每个模态文件仅通过 `routeSpec` 提供其不同之处 - 路径、源协议、模态及其缓存阶段。特定于模态的阶段（例如未来的图像多部分解析器）被添加为 `routeSpec` 字段，而不是通过重新内联整个链。

## 2.RequestContext 存储

`RequestContext` 通过 `context.WithValue` 连接到 `c.Request.Context()`，而不是 `gin.Context.Set/Get`。

录入助手：

- `middleware.AttachRequestContext(c, rc)`：仅由M1调用。
- `middleware.GetRequestContext(c)`：如果未找到，则发生恐慌，由 M9 Recover 支持。

这样，请求状态、OTel SpanContext 和 Baggage 都包含在同一个 stdlib 上下文容器中。

## 3.`internal/requeststate.State`

目标定义：

```go
type State struct {
    RequestID string
    StartTime time.Time

    Identity UserIdentity
    Envelope *RequestEnvelope
    Handlers protocol.Lookup

    ModelService *ModelService // model from the original request
    ModelChain   []*ModelService // sequence of attempts pre-resolved by M5: primary + validated fallbacks
    RoutedModelService *ModelService // the model that actually succeeded; equals ModelService when no fallback occurred

    Endpoint *Endpoint

    Usage              *Usage
    Error              *AdapterError
    SchedulingDecision *SchedulingDecision
}
```

重要限制：

- `trace_id` / `span_id` 不存储为字段；它们是从 `c.Request.Context()` (`middleware.TraceIDFromCtx`) 内的 OTel 上下文中提取的。
- **`context.Context` 未附加到 RC** — 唯一的事实来源是 `c.Request.Context()`。中间件通过 `c.Request.Context()` 读取 ctx 并通过`c.Request = c.Request.WithContext(ctx)`。将 ctx 字段附加到可变结构违反了 Go 的“上下文是值，而不是状态”原则，并且会偏离 gin 的原生 `c.Request.Context()`。
- `*gin.Context` 未存储；响应写入是由中间件使用当前处理器的 `c.Writer` 完成的。
- 禁止使用无类型扩展映射；新状态必须有明确的类型和所有者。
- `*slog.Logger` 未存储；日志记录使用 `slog.*Context` 方法，并且 `trace.CtxHandler` 自动填充跟踪/行李字段。
- 业务代码必须使用`slog.InfoContext` / `ErrorContext` / `WarnContext`等上下文承载方法；禁止在请求路径中直接调用 `slog.Info` / `Error`，否则无法注入跟踪字段。
- M4预算不会将`BudgetStatus`写入RC；它要么通过并继续，要么失败并中止。
- 适配器会话未附加到 RC；仅保留响应阶段工件 `Usage`、`Error` 和 `SchedulingDecision`。

## 4. 中间件链

|编号|名称 |主要输入|主要输出/副作用|
|------|------|----------|-----------------|
| pre | BodyLimit | 配置 `middleware.body_limit_bytes` | 超限时返回 413 |
|预|超时 |配置`middleware.timeout` |设置请求上下文超时 |
| M1 |跟踪上下文 | HTTP 标头/上下文 |创建 RC、RequestID、跨度/行李 |
| M9 | Recover | RC / 错误 / panic | 统一错误 JSON，提供 panic 兜底 |
| M2|授权 |授权/X-API-Key | `rc.Identity` |
| M3 |请求信封|请求体+路由协议标签| `rc.Envelope`，正文可重读 |
| M4 | Budget | `rc.Identity` | 门控失败时中止 |
| M5 | ModelService | `rc.Envelope.Model`、主账户 pin、`X-Gateway-Fallback-Models` | `rc.ModelService`（主模型）、`rc.ModelChain`（主模型 + 已验证的回退模型） |
| M6|限制|身份、型号、配额策略|用户侧RPM/RPS预扣；事后TPM事后扣除|
| M8|策略执行|可信身份+请求/响应内容|强制执行可插拔 `allow`/`deny`/`redact` 决策；旧版审核已调整（M6 之后：429 绑定请求不得花费外部策略调用）|
| — |缓存|请求正文/提示|响应缓存（仅限聊天+嵌入模式）；命中时直接返回并跳过 M7； `cache.enabled=false` 时无操作 |
| M7 | Schedule | 模型、组、候选端点 | `rc.RoutedModelService`、端点、上游转发、用量、决策 |
| M10 | Tracing | 最终 RC 状态 | 指标、用量 Outbox、调度 trace |

M9 在 M10 内部和 M2 Auth 外部注册，因此它的延迟覆盖了 M2 和每个内部中间件/处理器。 BodyLimit、Timeout 和 M1 仍然在 M9 之外，并受到 Gin 全局回退恢复的保护。

M8使用可信API密钥/账户解决了有效的不可变策略
身份，在 M7 看到请求信封之前应用输入突变，安装
输出控制器，然后在 `c.Next()` 返回后记录执行结果。
因此，严格的输出缓冲仍然由 M8 拥有，即使实际的
流装饰器在调用者转发内部执行。

**M10注册在M1之后、M9之前**（其整理逻辑运行在后`c.Next()`洋葱返回阶段）：
- 如果任何后续中间件中止 (401/429/503) → 完成逻辑仍在回程中运行 - 请求指标/
  使用事件/M10 拥有的决策审计**没有盲点**（在链尾部注册的旧版本将是
  `c.Abort()` 完全跳过，让诸如凭证填充/速率限制风暴之类的东西隐形地隐藏在
  请求指标）
- 发生恐慌时 → 内部 M9 首先恢复并写入 500，控制流正常返回，M10 的整理逻辑看到最终的 500 状态

## 5.M2 认证

目标依赖项由 M2 本身声明为最小接口，repo 仅作为实现。 API 密钥存储为 SHA-256 哈希值；解析后得到 `domain.UserIdentity`：

```go
type UserIdentity struct {
    AccountID            string // primary account pin / billing entity
    SubAccountID         string // sub-account / operator
    APIKeyID             string
    Group                string
    ExternalUser         bool
    AccountQuotaPolicyID *int64 // primary-account-level rate limit policy
    APIKeyQuotaPolicyID *int64 // API-key-level rate limit policy
}
```

`AccountID` 为主账户 pin / 计费实体；`SubAccountID` 是该主账户下的子账户/操作员。`AccountQuotaPolicyID` 来自 `accounts` join，`APIKeyQuotaPolicyID` 来自 `api_keys`。M6 使用这两个 ID，避免每次请求都重复查询主账户和 API key 的策略关系。

API密钥查找基于`key_hash`上的唯一索引；一旦哈希命中，`AccountID`、`SubAccountID`，并且策略ID从accounts/api_keys中加入出来——调用者不能通过标头改变所有权。 `SubAccountID` 通过 API 密钥记录关联，并且不直接从请求标头信任。理论上的哈希冲突被视为系统错误，返回 500/503 并发出警报。

一旦 M2 完成凭证解析，后续日志、跟踪行李、上游转发和内容日志不得携带原始 `Authorization` / `X-API-Key` 值；当需要通过上游身份验证时，仅从端点身份验证配置重新生成标头。

## 6.M3请求信封

`RequestEnvelope`仅携带路由所需的最少信息：

```go
type RequestEnvelope struct {
    RawBytes       []byte
    Model          string
    SourceProtocol Protocol
    Modality       Modality
}
```

M3不进行规范化，也不解析全套协议字段。请求体型转换由`internal/translator`处理。

## 7.M5 模型服务

M5 使用中间件拥有的接口，由来自 `internal/repo` 的缓存包装器注入：

- `ModelCatalog.GetByModel` 查找全局模型目录（生产实现：`repo.CachedModelServiceReader`
  TTL LRU 命中时直接返回；如果错过，则变为 `repo.SQLModelServiceReader.GetByModel`； TTL 默认为 30 秒）。
- `SubscriptionChecker.HasModel` 检查主账户是否订阅了该模型（也通过缓存包装器）。

`model_services` 是全局目录，不再按主账户存储；每个主账户的可见性由 `account_model_subscriptions` 表确定。

5 个缓存包装器的列表及其默认参数位于 [06 §8.2](./06-pluggable-infra.zh-CN.md#82-applicable-tables-and-default-parameters).

M5不查询活跃定价，也不向`RequestContext`写入定价快照。价格匹配和金额计算由下游计费平台根据Usage Event中的请求发生时间进行。

M5 中也完成了跨模型回退验证：它解析 `X-Gateway-Fallback-Models` 标头（有关详细信息，请参阅 [03 §5](./03-endpoint-scheduling.zh-CN.md#5-retry-model)），并且对于每个回退模型重新运行目录 + 订阅检查，写入经过验证的`*ModelService` 条目进入 `rc.ModelChain`（主要位于 `[0]`，回退按顺序附加）。不存在或未订阅的回退将被悄悄删除 - 只要主成功，请求就会继续。 M7瘦适配器将`rc.ModelChain`投影为`dispatch.Input`，由`dispatch.Dispatcher`消耗以决定是否切换到回退模型； M7仅将`dispatch.Outcome`写回到`rc.RoutedModelService` / `rc.Usage` / `rc.SchedulingDecision`。使用元和尝试记录都使用实际路由的模型。

SQL 查询失败统一视为依赖项失败：当 M2 IdentityResolver、M5 ModelCatalog、SubscriptionChecker 或调度 `CandidateSource`（在生产中桥接到存储库 EndpointReader）返回数据库错误时，它们会失败关闭，并响应 503 和`Retry-After`，并且不得伪装为401/403/404。 PolicyCache 的显式 TTL 缓存是一个例外；在缓存命中时，缓存的值可能会继续使用，但在缓存未命中后，数据库故障仍会返回 503。

## 8. 错误退出

早期中间件写入`rc.Error`，通过内部`abort(c, status, class, message)`调用`c.Abort()`。M9 Recover统一转换`rc.Error` 进入响应。

一旦 M7 响应流启动，后续错误将无法再覆盖 HTTP 状态；此时仅写入 `rc.Error`，用于日志记录、指标和Outbox参考。

统一的错误响应正文包装在顶级 `error` 字段中，以便将来可以轻松地使用非错误返回字段进行扩展：

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

限制条件：

- `Code` 是稳定的、机器可读的错误代码，例如`rate_limit_exceeded`、`invalid_request`、`no_endpoint_available`。
- `Message` 是人类可读的描述，不得用作程序逻辑的基础。
- `Class` 与调度/上游错误分类一致，例如`invalid`、`rate_limit`、`transient`。
- `Details` 应仅包含故障排除所需的字段，例如限速维度、桶键、端点id；它不得包含请求/响应正文。
- `RequestID` / `TraceID` 由来自 `rc` 的 M9 恢复填充；客户端可以使用它们与日志/跟踪关联，而不需要单独的标头。

有关 JSON 示例，请参阅 [08-可观察性 §7](./08-observability.zh-CN.md#7-error-response)。

## 9. 框架边界

本文档中出现的 `c.Next()` 指的是 gin 洋葱模型的后处理阶段；目标合约是“预处理->下游处理器/中间件->后处理整理”，而不是要求业务逻辑依赖于gin API。如果 HTTP 框架被替换，则必须保留此执行语义。

<a id="10-middleware-assembly-contract-aligned-with-otelgin-v0680"></a>
## 10. 中间件组装合约（与 otelgin v0.68.0 一致）

每个中间件公开：

- 一个 `XxxOption` 接口 + 一个私有 `xxxOptionFunc` 适配器（不再是功能类型
  `func(*cfg)`）。
- 所需依赖项的 `WithXxxFoo(...)` 构造函数，如果缺少，则在构造时会出现恐慌。
- `WithXxxBar(...)` 用于可选依赖项，当 nil 时有明确的默认值（例如 nil 审核器 = 传递）。
- `WithXxxTracerProvider(tp oteltrace.TracerProvider)`：注入OTel TracerProvider；
  当 nil 时，启动时回落到 `otel.GetTracerProvider()`。

中间件启动时一次性设置：

```go
cfg := xxxConfig{}
for _, opt := range opts { opt.apply(&cfg) }
if cfg.required == nil { panic("middleware.Xxx: WithXxxRequired required") }
if cfg.tracerProvider == nil { cfg.tracerProvider = otel.GetTracerProvider() }
tracer := cfg.tracerProvider.Tracer(ScopeName)   // held by closure
return func(c *gin.Context) { /* hot path */ }
```

标准热路径模板（ctx 传播的单一真实来源是 `c.Request.Context()`，**不是** RC 字段）：

```go
return func(c *gin.Context) {
    ctx, span := tracer.Start(c.Request.Context(), "xxx.action")
    defer span.End()
    c.Request = c.Request.WithContext(ctx)   // ← downstream mw automatically picks up the span

    rc := GetRequestContext(c)               // RC only carries data, not ctx
    // ... all business calls pass the local ctx: cfg.dep.Call(ctx, ...)
}
```

直通快速路径（直通审核器/无预算门/无速率限制
store) 在启动时已经返回 `func(c) { c.Next() }`，甚至没有打开跟踪器。参见
[06 §6 中间件选项](./06-pluggable-infra.zh-CN.md#6-middleware-options) 了解详细信息。

M1 TraceContext 是参考实现，另外公开了 `WithTraceContextPropagators` /
`WithSpanNameFormatter` / `WithTraceContextSpanStartOptions`，完全镜像otelgin的
`WithPropagators` / `WithSpanNameFormatter` / `WithSpanStartOptions`。

## 11. 演进规则

- 添加新的 RC 字段时，必须记录写入中间件和读取器。与跨模型回退相关的字段必须区分原始请求模型和实际路由模型。
- 当添加新的中间件时，更新本文档中的排序表，并检查是否所有模态路由都需要采用它。
- 请求路径日志记录应添加 lint/测试约束，扫描非上下文调用，例如 `slog.Info(`、`slog.Error(`、`slog.Warn(`。
- 不要向请求状态添加临时的无类型字段；只提倡具有明确所有权的稳定、类型化的国家。
