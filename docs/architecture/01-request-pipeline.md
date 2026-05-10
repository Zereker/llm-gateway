# 01 — Request Pipeline

本文定义 `domain.RequestContext` 数据结构与 10 个 middleware 的完整契约（输入、输出、错误、副作用、顺序约束）。是其他主题（02-05）的前置依赖。

> **阅读前**：[00-overview](00-overview.md) 第 3 章设计原则；尤其 P2（typed `domain.RequestContext` + gin.Context.Set 模式）和 P6（预检与扣减分离）。

## 1. 包结构

代码包按主题切分，与本目录的文档章节一一对应。每个 domain 包只定义自己的领域类型与接口，不互相 import。`domain.RequestContext` 集中聚合所有 domain 类型，由 `middleware` 包消费。

```
pkg/
├── request/                    # 跨 middleware 的请求级状态
│   ├── context.go              # type Context 聚合所有 domain 类型
│   └── helpers.go              # From / tryFrom / Attach + 私有 ctxKey 常量
│
├── envelope/                   # 02 协议转换的领域类型
│   ├── envelope.go             # Envelope, CanonicalRequest
│   ├── protocol.go             # SourceProtocol enum
│   └── modality.go             # Modality enum
│
├── identity/                   # M2 产物
│   └── user.go                 # User
│
├── budget/                     # M4 产物
│   └── status.go               # Status enum + 常量
│
├── modelservice/               # M5 产物
│   ├── snapshot.go             # Snapshot
│   └── pricing.go              # PricingSnapshot（价格指纹，不含价格本体）
│
├── limit/                      # 04 限流
│   ├── spec.go                 # Spec
│   └── checker.go              # Checker 接口、CheckResult
│
├── scheduling/                 # 03 端点选择
│   ├── endpoint.go             # Endpoint
│   ├── decision.go             # Decision
│   ├── scheduler.go            # Scheduler 接口
│   └── retry_executor.go       # RetryExecutor 接口
│
├── usage/                      # 05 计量
│   ├── usage.go                # Usage
│   └── bus.go                  # EventBus 接口
│
├── errs/                       # 统一错误模型
│   ├── class.go                # Class enum + 常量
│   └── error.go                # Error struct
│
├── adapter/                    # 02 协议转换的厂商适配
│   ├── adapter.go              # Adapter 接口、ResponseSession 接口
│   ├── registry.go             # init() 注册表
│   ├── openai/                 # 厂商子包
│   ├── anthropic/
│   └── ...
│
├── middleware/                 # M1-M10 实现
│   ├── chain.go                # Register + Deps + Validate
│   ├── trace_context.go        # M1
│   ├── auth.go                 # M2
│   ├── envelope.go             # M3
│   ├── budget.go               # M4
│   ├── model_service.go        # M5
│   ├── limit.go                # M6
│   ├── moderation.go           # M8
│   ├── schedule.go             # M7
│   ├── recover.go              # M9
│   └── tracing.go              # M10
│
└── metric/
    └── names.go                # Prometheus metric 常量
```

**依赖方向**：

```
middleware  ──┬─→ request ──┬─→ envelope
              │              ├─→ identity
              │              ├─→ budget
              │              ├─→ modelservice
              │              ├─→ limit
              │              ├─→ scheduling
              │              ├─→ usage
              │              ├─→ errs
              │              └─→ adapter
              │
              ├─→ envelope, identity, budget, modelservice, limit, scheduling, usage, errs, adapter（按需）
              │
              └─→ infra（middleware.IdentityProvider / middleware.BudgetGate 等抽象，详见 [06]）

domain 包之间互不 import；request 单向 import 它们；middleware 既 import request 也 import 各 domain 包。
```

## 2. domain.RequestContext 定义

```go
// pkg/domain/context.go
package request

import (
    "context"
    "log/slog"
    "time"

    "github.com/gin-gonic/gin"

    "github.com/zereker/llm-gateway/pkg/adapter"
    "github.com/zereker/llm-gateway/pkg/domain"
    "github.com/zereker/llm-gateway/pkg/ratelimit"
    "github.com/zereker/llm-gateway/pkg/domain"
    "github.com/zereker/llm-gateway/pkg/schedule"
    "github.com/zereker/llm-gateway/pkg/usage"
)

// Context 是一次 HTTP 请求的全链路可变状态。
//
// 写入规则：
//   - 每个字段标注了写入它的 middleware（M1-M10）
//   - 后注册的 middleware 不应改写前者已写入的字段（除注释明确允许）
//   - Handler / Adapter 视为只读消费者；Usage / Error / SchedulingDecision 是响应阶段产物
//
// 读取规则：
//   - 通过 middleware.GetRequestContext(c) 取出，杜绝裸调 c.MustGet/c.Get
//
// 演进规则：
//   - 字段增删需同步更新本文档第 2 节
//   - 临时性、实验性字段走 Extras map，不污染 struct
type Context struct {
    // ===== M1 TraceContext 写入 =====
    TraceID   string    // 客户端可通过 X-Trace-Id header 透传；缺失则生成
    RequestID string    // 单次请求内唯一（含 retry 时的子请求由 Scheduler 自行衍生）
    StartTime time.Time // 请求进入网关时间

    // ===== M2 Auth 写入 =====
    Identity domain.UserIdentity // 鉴权后的身份；ExternalUser 为 false 时 M4 可短路

    // ===== M3 Envelope 写入 =====
    Envelope *domain.RequestEnvelope // 解析后的请求信封，详见 [02]

    // ===== M4 Budget 写入 =====
    BudgetStatus domain.BudgetStatus // domain.BudgetActive 才会进入后续；其他值 M4 内 abort

    // ===== M5 ModelService 写入 =====
    ModelService *domain.ModelServiceSnapshot   // 模型路由配置（含 endpoint 池标识、限流默认值等）
    Pricing      domain.PricingSnapshot // 仅含价格指纹（ID + UpdateTime），详见 [05]

    // ===== M6 Limit 写入 =====
    LimitSpec *domain.LimitSpec // 三层阈值，M10 后置 Consume 复用

    // ===== M7 Schedule 写入 =====
    Endpoint *domain.Endpoint // 实际选中的 endpoint（重试 / fallback 后是最后一个）
    Adapter  adapter.Adapter      // 与 Endpoint 对应的 Adapter 实例

    // ===== 响应阶段写入（M7 内部 / Adapter） =====
    Usage              *domain.Usage         // 真实 token 用量；流式终态填入；详见 [05]
    Error              *domain.AdapterError          // 终态错误（已分类）；详见第 7 节
    SchedulingDecision *domain.SchedulingDecision // 整次调度决策（含每次尝试的 endpoint 与结果）；详见 [03]

    // ===== 全链路共享（M1 写入，后续只读） =====
    Ctx    context.Context // 业务 context；用于 timeout / cancel
    GinCtx *gin.Context    // HTTP 写器（Adapter 流式写出时需要）
    Logger *slog.Logger    // 已带 trace_id / user_id 等基础字段

    // ===== 扩展点 =====
    Extras map[string]any // 临时性 / 实验性字段；正式字段必须升级到 struct
}
```

各 domain 包对应的领域类型简表（完整定义见各主题文档）：

```go
// pkg/domain/user.go
package identity

type User struct {
    UserID       string // 平台内的用户唯一标识（与 IdentityProvider 解耦，见 [06]）
    APIKeyID     string // 命中的 API Key 的 ID（用于审计与限流维度）
    Group        string // 限流 / 调度分组；默认 "default"，可扩展 "reserved"/"premium" 等
    ExternalUser bool   // true = 第三方付费用户（需走预算检查）；false = 内部 / 免费用户
}
```

```go
// pkg/domain/status.go
package middleware

type Status int

const (
    Unknown  Status = 0 // 全 miss 或检查未发生
    Active   Status = 1 // 通过
    Inactive Status = 2 // 欠费 / 订阅过期 / 配额耗尽
)
```

```go
// pkg/domain/snapshot.go
package modelservice

import (
    "encoding/json"
    "time"
)

// Snapshot 是模型对外暴露的路由配置
type Snapshot struct {
    ID         int64           // 内部唯一 ID（用于计量事件）
    ServiceID  string          // 业务唯一标识（如 "openai/gpt-4o"）
    Model      string          // 客户端可见的模型名（如 "gpt-4o"）
    UpdateTime time.Time       // 配置最后更新时间；与 ID 共同构成 PricingSnapshot 指纹
    SpecDetail json.RawMessage // 计量计价详细规格的 JSON；按需解析；详见 [05]
    Group      string          // 默认 endpoint 组（供 [03] Scheduler 路由）
    Tpm        int64           // 默认每分钟 token 限额（供 [04] 限流）
    Rpm        int64           // 默认每分钟请求数限额
}
```

```go
// pkg/domain/pricing.go
package modelservice

import "time"

// PricingSnapshot 价格快照的指纹（不含价格本体）
//
// 计量事件只携带指纹（约 50 字节），下游 Enrich 阶段按指纹查 history 表拿真实价格。
// 详见 [05-metering-billing]。
type PricingSnapshot struct {
    ModelServiceID    int64
    ServiceUpdateTime time.Time
}
```

> `domain.RequestEnvelope` / `domain.LimitSpec` / `domain.Endpoint` / `domain.SchedulingDecision` / `adapter.Adapter` / `domain.Usage` 的完整定义见各自主题文档（02-05）。

## 3. Helper 函数

`gin.Context` 的 key 常量私有化在 `request` 包内，外部不可直接访问；统一通过 `From` / `Attach` 操作。

```go
// pkg/domain/helpers.go
package request

import "github.com/gin-gonic/gin"

const ctxKey = "llm_gateway.request_context"

// Attach 将 *Context 挂到 *gin.Context 上。仅 M1 TraceContext middleware 调用。
func Attach(c *gin.Context, rc *Context) {
    c.Set(ctxKey, rc)
}

// From 从 *gin.Context 取出 *Context。
// 假设 M1 已注册并已执行（M1 是链中第一个 middleware，未注册即配置错误，启动自检会拦下）。
// 若取不到则 panic — 由 M9 Recover 兜底转 500。
func From(c *gin.Context) *Context {
    v, ok := c.Get(ctxKey)
    if !ok {
        panic("domain.RequestContext not set: M1 TraceContext middleware missing")
    }
    return v.(*Context)
}

// tryFrom 是 From 的安全版：取不到返回 nil，专供 Recover 使用。
func tryFrom(c *gin.Context) *Context {
    v, ok := c.Get(ctxKey)
    if !ok {
        return nil
    }
    return v.(*Context)
}
```

> **不要**在业务代码里裸调 `c.MustGet` / `c.Get`，统一走 `middleware.GetRequestContext(c)`。
> Recover middleware 是唯一例外（它需要在 RC 不存在时也能兜底），用 `tryFrom`。

## 4. Middleware 契约总览

| ID | 名称 | 写入字段 | 读取字段 | 可能 abort | 错误码 |
|----|------|---------|---------|----------|-------|
| M1 | TraceContext | TraceID, RequestID, StartTime, Ctx, GinCtx, Logger, Extras | — | 不 | — |
| M9 | Recover | Error（panic 兜底） | 全部 | 不（捕获 panic 后写错误） | 500 |
| M2 | Auth | Identity, Logger（追加 user_id） | TraceID | ✅ | 401 |
| M3 | Envelope | Envelope | — | ✅ | 400 |
| M4 | Budget | BudgetStatus | Identity | ✅ | 403 |
| M5 | ModelService | ModelService, Pricing | Envelope.Parsed.Model | ✅ | 404 |
| M6 | Limit | LimitSpec, Extras["service_blocked"] | Identity, ModelService | ✅（仅用户层）| 429 |
| M8 | ContentModeration | Extras["moderation_enabled"] | Envelope | ✅（仅违规） | 400 |
| M7 | Schedule | Endpoint, Adapter, Usage, Error, SchedulingDecision | 全部 | 不（错误进 Error） | — |
| M10 | Tracing | — | 全部 | 不 | — |

**注册顺序**（实际 gin.Use 调用顺序）：

```
M1 → M9 → M2 → M3 → M4 → M5 → M6 → M8 → M7 → M10
```

> M9 Recover 紧跟 M1 注册，是为了让 panic 能被捕获到（gin 的 defer 链顺序）。
> M10 Tracing 实际是 defer 执行，名义上"最后"。

## 5. 各 Middleware 契约

### M1 TraceContext

**职责**：构建 `domain.RequestContext`，挂到 `gin.Context`。

**前置**：无。
**后置**：`rc.TraceID` / `rc.RequestID` / `rc.StartTime` / `rc.Ctx` / `rc.GinCtx` / `rc.Logger` / `rc.Extras` 已就绪。
**错误**：不 abort。
**幂等**：是（同请求多次执行会重置 trace_id，应避免）。

```go
// pkg/middleware/trace_context.go
package middleware

import (
    "log/slog"
    "time"

    "github.com/gin-gonic/gin"

    "github.com/zereker/llm-gateway/pkg/domain"
)

func TraceContext() gin.HandlerFunc {
    return func(c *gin.Context) {
        traceID := c.GetHeader("X-Trace-Id")
        if traceID == "" {
            traceID = genTraceID()
        }
        rc := &domain.RequestContext{
            TraceID:   traceID,
            RequestID: genRequestID(),
            StartTime: time.Now(),
            Ctx:       c.Request.Context(),
            GinCtx:    c,
            Logger:    slog.Default().With("trace_id", traceID),
            Extras:    make(map[string]any),
        }
        middleware.AttachRequestContext(c, rc)
        c.Next()
    }
}
```

### M9 Recover

**职责**：捕获后续 middleware / handler 的 panic；统一错误响应。

**前置**：M1 已写入 `rc.Logger`（用于 panic 日志）。
**后置**：若 `rc.Error != nil` 且响应未写出，写出错误响应。
**错误**：自身不 abort；将 panic 转成 500 响应。

```go
// pkg/middleware/recover.go
package middleware

import (
    "runtime/debug"

    "github.com/gin-gonic/gin"

    "github.com/zereker/llm-gateway/pkg/domain"
    "github.com/zereker/llm-gateway/pkg/metric"
    "github.com/zereker/llm-gateway/pkg/domain"
)

func Recover() gin.HandlerFunc {
    return func(c *gin.Context) {
        defer func() {
            if r := recover(); r != nil {
                metric.Inc(metric.PanicTotal, "component", "middleware")
                if rc := middleware.TryGetRequestContext(c); rc != nil {
                    rc.Logger.Error("panic", "recover", r, "stack", debug.Stack())
                }
                writeError(c, &domain.AdapterError{
                    Class:      domain.ErrUnknown,
                    HTTPStatus: 500,
                    Message:    "internal server error",
                })
            }
        }()
        c.Next()

        // 主链结束后兜底处理 Error（M7 等填入但未写出）
        if rc := middleware.TryGetRequestContext(c); rc != nil && rc.Error != nil && !c.Writer.Written() {
            writeError(c, rc.Error)
        }
    }
}
```

> `middleware.TryGetRequestContext` 是 `tryFrom` 的导出版本（仅供 middleware 使用）；`writeError` 见第 7 节。

### M2 Auth

**职责**：从请求头提取凭证 → 调 `middleware.IdentityProvider` → 写 `rc.Identity`。

**前置**：M1 已执行。
**后置**：`rc.Identity` 字段全部就绪；`rc.Logger` 追加 `user_id` 字段。
**错误**：401（缺凭证 / 无效凭证）。

```go
// pkg/middleware/auth.go
package middleware

import (
    "github.com/gin-gonic/gin"

    "github.com/zereker/llm-gateway/pkg/domain"
    "github.com/zereker/llm-gateway/pkg/middleware" // 见 [06]
    "github.com/zereker/llm-gateway/pkg/metric"
    "github.com/zereker/llm-gateway/pkg/domain"
)

type AuthDeps struct {
    Provider middleware.IdentityProvider
}

func Auth(deps AuthDeps) gin.HandlerFunc {
    return func(c *gin.Context) {
        rc := middleware.GetRequestContext(c)

        creds := extractCredentials(c) // 从 Authorization / X-API-Key 等头部提取
        if creds == nil {
            abort(c, 401, "missing credentials")
            return
        }

        u, err := deps.Provider.Resolve(rc.Ctx, creds)
        if err != nil {
            metric.Inc(metric.AuthTotal, "result", "invalid")
            abort(c, 401, "invalid credentials: "+err.Error())
            return
        }

        rc.Identity = domain.UserIdentity{
            UserID:       u.UserID,
            APIKeyID:     u.APIKeyID,
            Group:        u.Group,
            ExternalUser: u.External,
        }
        rc.Logger = rc.Logger.With("user_id", u.UserID)
        c.Next()
    }
}
```

### M3 Envelope

**职责**：读 body、识别协议与 modality、解析为 `CanonicalRequest`。

**前置**：M1, M2 已执行。
**后置**：`rc.Envelope` 字段全部就绪。
**错误**：400（body 读取失败 / 协议解析失败）。

详细的 `domain.RequestEnvelope` / `SourceProtocol` / `Modality` 枚举与解析规则见 [02-protocol-translation](02-protocol-translation.md)。

```go
// pkg/middleware/envelope.go
package middleware

import (
    "io"

    "github.com/gin-gonic/gin"

    "github.com/zereker/llm-gateway/pkg/domain"
)

type EnvelopeDeps struct {
    Detector middleware.Detector
    Parser   middleware.Parser
}

func Envelope(deps EnvelopeDeps) gin.HandlerFunc {
    return func(c *gin.Context) {
        rc := middleware.GetRequestContext(c)

        raw, err := io.ReadAll(c.Request.Body)
        if err != nil {
            abort(c, 400, "read body failed")
            return
        }

        sourceProto, modality := deps.Detector.Detect(c.Request.URL.Path, raw)
        if sourceProto == domain.ProtoUnknown {
            abort(c, 400, "unknown source protocol")
            return
        }

        parsed, err := deps.Parser.Parse(raw, sourceProto, modality)
        if err != nil {
            abort(c, 400, "parse body failed: "+err.Error())
            return
        }

        rc.Envelope = &domain.RequestEnvelope{
            RawBytes:       raw,
            Parsed:         parsed,
            SourceProtocol: sourceProto,
            Modality:       modality,
            RequestTime:    rc.StartTime,
        }
        c.Next()
    }
}
```

### M4 Budget

**职责**：检查用户预算 / 配额状态。

**前置**：M2 写入了 `rc.Identity`。
**后置**：`rc.BudgetStatus` 已确定。
**错误**：403（不通过）；`domain.BudgetUnknown` 默认放行 + 告警（避免依赖失效导致全量拒绝）。

```go
// pkg/middleware/budget.go
package middleware

import (
    "github.com/gin-gonic/gin"

    "github.com/zereker/llm-gateway/pkg/domain"
    bud "github.com/zereker/llm-gateway/pkg/middleware" // 见 [06]：抽象接口
    "github.com/zereker/llm-gateway/pkg/metric"
    "github.com/zereker/llm-gateway/pkg/domain"
)

type BudgetDeps struct {
    Checker bud.Checker
}

func Budget(deps BudgetDeps) gin.HandlerFunc {
    return func(c *gin.Context) {
        rc := middleware.GetRequestContext(c)

        // 内部用户跳过
        if !rc.Identity.ExternalUser {
            rc.BudgetStatus = domain.BudgetActive
            c.Next()
            return
        }

        status, err := deps.Checker.Check(rc.Ctx, rc.Identity.UserID)
        if err != nil {
            // 检查失败：默认放行 + 告警，避免基础设施挂掉拖垮全量请求
            metric.Inc(metric.BudgetCheckTotal, "result", "fallback_pass")
            rc.Logger.Warn("budget check failed, fallback pass", "err", err)
            rc.BudgetStatus = domain.BudgetActive
            c.Next()
            return
        }

        rc.BudgetStatus = status
        switch status {
        case domain.BudgetActive:
            metric.Inc(metric.BudgetCheckTotal, "result", "pass")
            c.Next()
        case domain.BudgetInactive:
            metric.Inc(metric.BudgetCheckTotal, "result", "blocked")
            abort(c, 403, "budget inactive")
        }
    }
}
```

### M5 ModelService

**职责**：根据 `Envelope.Parsed.Model` 加载模型路由配置 + 价格指纹。

**前置**：M3 写入了 `rc.Envelope`。
**后置**：`rc.ModelService` 与 `rc.Pricing` 全部就绪。
**错误**：404（模型未注册）。

```go
// pkg/middleware/model_service.go
package middleware

import (
    "github.com/gin-gonic/gin"

    "github.com/zereker/llm-gateway/pkg/domain"
)

type ModelServiceDeps struct {
    Loader modelservice.Loader // 接口；底层走 ConfigStore + LRU 缓存，见 [06]
}

func ModelService(deps ModelServiceDeps) gin.HandlerFunc {
    return func(c *gin.Context) {
        rc := middleware.GetRequestContext(c)

        ms, err := deps.Loader.GetByModel(rc.Ctx, rc.Envelope.Parsed.Model)
        if err != nil {
            abort(c, 404, "model not found: "+rc.Envelope.Parsed.Model)
            return
        }

        rc.ModelService = ms
        rc.Pricing = domain.PricingSnapshot{
            ModelServiceID:    ms.ID,
            ServiceUpdateTime: ms.UpdateTime,
        }
        c.Next()
    }
}
```

### M6 Limit

**职责**：构建 `domain.LimitSpec` + 三层限流预检（read-only）。

**前置**：M2, M5 已执行。
**后置**：
- `rc.LimitSpec` 已就绪（M10 Consume 时复用）；
- 用户层超限 → abort 429；
- 模型层超限 → 不 abort，写 `rc.Extras["service_blocked"] = true` 让 M7 RetryExecutor 决定 fallback；
- endpoint 层超限 → 不在 M6 处理（M7 内的 Filter 链处理）。

详见 [04-rate-limiting](04-rate-limiting.md)。

```go
// pkg/middleware/limit.go
package middleware

import (
    "github.com/gin-gonic/gin"

    "github.com/zereker/llm-gateway/pkg/ratelimit"
    "github.com/zereker/llm-gateway/pkg/metric"
    "github.com/zereker/llm-gateway/pkg/domain"
)

type LimitDeps struct {
    Checker ratelimit.Checker
}

func Limit(deps LimitDeps) gin.HandlerFunc {
    return func(c *gin.Context) {
        rc := middleware.GetRequestContext(c)

        spec := deps.Checker.BuildSpec(rc.Identity, rc.ModelService) // 四级查询链
        rc.LimitSpec = spec

        result := deps.Checker.CheckReadOnly(rc.Ctx, spec, rc.Identity, rc.ModelService)
        if result.UserBlocked {
            metric.Inc(metric.RateLimitCheckTotal, "layer", "user", "result", "blocked")
            abort(c, 429, "user rate limit exceeded")
            return
        }
        if result.ServiceBlocked {
            metric.Inc(metric.RateLimitCheckTotal, "layer", "service", "result", "blocked")
            rc.Extras["service_blocked"] = true // M7 RetryExecutor 会读它
        }
        c.Next()
    }
}
```

### M8 ContentModeration

**职责**：请求内容审核（响应内容审核由 M7 内 ResponseSession 处理）。

**前置**：M3 写入了 `rc.Envelope`。
**后置**：违规则 abort；通过则在 `rc.Extras["moderation_enabled"] = true`，告知 M7 包装响应审核。
**错误**：400（违规）。

```go
// pkg/middleware/moderation.go
package middleware

import (
    "github.com/gin-gonic/gin"

    "github.com/zereker/llm-gateway/pkg/middleware" // 见 [06]
    "github.com/zereker/llm-gateway/pkg/domain"
)

type ModerationDeps struct {
    Moderator middleware.Moderator // 可为 nil（NoOp）
}

func ContentModeration(deps ModerationDeps) gin.HandlerFunc {
    return func(c *gin.Context) {
        rc := middleware.GetRequestContext(c)

        if deps.Moderator == nil {
            c.Next()
            return
        }
        if err := deps.Moderator.CheckInput(rc.Ctx, rc.Envelope); err != nil {
            abort(c, 400, "content moderation rejected: "+err.Error())
            return
        }
        rc.Extras["moderation_enabled"] = true
        c.Next()
    }
}
```

### M7 Schedule

**职责**：选 endpoint → 调 Adapter → 流式响应；失败按策略 retry / fallback。

**前置**：M5, M6 已执行。
**后置**：`rc.Endpoint`、`rc.Adapter`、`rc.Usage`、`rc.SchedulingDecision` 已写入；失败时 `rc.Error` 已写入。
**错误**：不 abort；错误进 `rc.Error` 由 M9 兜底响应。

详见 [03-endpoint-scheduling](03-endpoint-scheduling.md)。

```go
// pkg/middleware/schedule.go
package middleware

import (
    "github.com/gin-gonic/gin"

    "github.com/zereker/llm-gateway/pkg/metric"
    "github.com/zereker/llm-gateway/pkg/domain"
    "github.com/zereker/llm-gateway/pkg/schedule"
)

type ScheduleDeps struct {
    Scheduler schedule.Scheduler
    Executor  schedule.RetryExecutor
}

func Schedule(deps ScheduleDeps) gin.HandlerFunc {
    return func(c *gin.Context) {
        rc := middleware.GetRequestContext(c)
        if err := deps.Executor.Run(c, rc); err != nil {
            // RetryExecutor 已将分类错误写入 rc.Error；这里只记录失败次数
            metric.Inc(metric.ScheduleResultTotal, "result", "failed")
        }
    }
}
```

### M10 Tracing

**职责**：聚合 metric、发送计量事件、写 trace。**实际执行在 c.Next() 之后**（defer 模式）。

**前置**：所有前置 middleware 已执行（无论成功失败）。
**后置**：metric、Usage 事件、trace 已落出。
**错误**：自身不影响响应；只做 best-effort。

```go
// pkg/middleware/tracing.go
package middleware

import (
    "strconv"
    "time"

    "github.com/gin-gonic/gin"

    "github.com/zereker/llm-gateway/pkg/trace" // 见 [06]
    "github.com/zereker/llm-gateway/pkg/metric"
    "github.com/zereker/llm-gateway/pkg/domain"
    "github.com/zereker/llm-gateway/pkg/usage"
)

type TracingDeps struct {
    UsageBus usage.OutboxPublisher // 默认本地文件 / 内存，见 [06]
    Tracer   trace.Tracer
}

func Tracing(deps TracingDeps) gin.HandlerFunc {
    return func(c *gin.Context) {
        c.Next() // 等所有后续 middleware 跑完

        rc := middleware.GetRequestContext(c)
        cost := time.Since(rc.StartTime).Milliseconds()

        metric.Observe(metric.HTTPRequestDurationMs,
            float64(cost),
            "method", c.Request.Method,
            "path", c.FullPath(),
            "status", strconv.Itoa(c.Writer.Status()))

        // 计量事件
        if rc.Usage != nil {
            _ = deps.UsageBus.Publish(rc.Ctx, usage.EventFrom(rc))
        }

        // 调度决策
        if rc.SchedulingDecision != nil {
            deps.Tracer.Log(rc.Ctx, "scheduling_decision", rc.SchedulingDecision)
        }
    }
}
```

## 6. Middleware 注册与启动自检

```go
// pkg/middleware/chain.go
package middleware

import (
    "errors"

    "github.com/gin-gonic/gin"
)

type Deps struct {
    Auth         AuthDeps
    Envelope     EnvelopeDeps
    Budget       BudgetDeps
    ModelService ModelServiceDeps
    Limit        LimitDeps
    Schedule     ScheduleDeps
    Moderation   ModerationDeps
    Tracing      TracingDeps
}

// Register 注册主链路 middleware。顺序固定，不可调整。
func Register(r *gin.Engine, deps Deps) {
    r.Use(
        TraceContext(),                     // M1：必须最先
        Recover(),                          // M9：紧随 M1，覆盖后续整链
        Auth(deps.Auth),                    // M2
        Envelope(deps.Envelope),            // M3
        Budget(deps.Budget),                // M4
        ModelService(deps.ModelService),    // M5
        Limit(deps.Limit),                  // M6
        ContentModeration(deps.Moderation), // M8
        Schedule(deps.Schedule),            // M7：终点
        Tracing(deps.Tracing),              // M10：实际 defer
    )
}

// Validate 启动自检：检查必需依赖是否齐全。
// 在 cmd/gateway/main.go 启动时调用：if err := deps.Validate(); err != nil { log.Fatal(err) }
func (d Deps) Validate() error {
    if d.Auth.Provider == nil {
        return errors.New("Auth.Provider missing")
    }
    if d.Envelope.Detector == nil || d.Envelope.Parser == nil {
        return errors.New("Envelope.Detector/Parser missing")
    }
    if d.Budget.Checker == nil {
        return errors.New("Budget.Checker missing")
    }
    if d.ModelService.Loader == nil {
        return errors.New("ModelService.Loader missing")
    }
    if d.Limit.Checker == nil {
        return errors.New("Limit.Checker missing")
    }
    if d.Schedule.Scheduler == nil || d.Schedule.Executor == nil {
        return errors.New("Schedule.Scheduler/Executor missing")
    }
    if d.Tracing.Tracer == nil || d.Tracing.UsageBus == nil {
        return errors.New("Tracing.Tracer/UsageBus missing (use NoOp impl if not needed)")
    }
    // Moderation.Moderator 允许为 nil（NoOp）
    return nil
}
```

## 7. 错误模型

### 错误分类

```go
// pkg/domain/class.go
package errs

type Class int

const (
    Unknown   Class = iota // 未知 / 兜底
    Invalid                // 客户端输入错误（4xx）
    Permanent              // 永久性失败（鉴权 / 配额 / 配置错误）
    Transient              // 暂时性失败（网络 / 上游 5xx / 超时），可重试
    RateLimit              // 限流（自身或上游），按 cooldown 重试
)

func (c Class) String() string {
    switch c {
    case Invalid:   return "invalid"
    case Permanent: return "permanent"
    case Transient: return "transient"
    case RateLimit: return "rate_limit"
    default:        return "unknown"
    }
}
```

### Error 结构

```go
// pkg/domain/error.go
package errs

type Error struct {
    Class           Class
    HTTPStatus      int    // 期望写出的 HTTP 状态；0 表示按 Class 默认
    Message         string // 给客户端的简短信息
    UpstreamMessage string // 上游原始 message（用于 debug；可选透传）
    Cause           error  // 底层 error（不暴露给客户端）
}

func (e *Error) Error() string { return e.Message }

// DefaultHTTPStatus 在 HTTPStatus 为 0 时按 Class 给默认。
func DefaultHTTPStatus(class Class) int {
    switch class {
    case Invalid:   return 400
    case Permanent: return 403
    case RateLimit: return 429
    case Transient: return 502
    default:        return 500
    }
}
```

### 响应体格式

统一为：

```json
{
  "error": {
    "code": "rate_limit",
    "message": "rate limit exceeded",
    "request_id": "req_abc123",
    "trace_id": "tr_xyz789"
  }
}
```

`code` 取自 `Class.String()`；`request_id` / `trace_id` 必带，便于客户端反馈定位。

写出实现：

```go
// pkg/middleware/recover.go (writeError)
func writeError(c *gin.Context, e *domain.AdapterError) {
    status := e.HTTPStatus
    if status == 0 {
        status = domain.DefaultHTTPStatus(e.Class)
    }
    body := gin.H{
        "error": gin.H{
            "code":    e.Class.String(),
            "message": e.Message,
        },
    }
    if rc := middleware.TryGetRequestContext(c); rc != nil {
        body["error"].(gin.H)["request_id"] = rc.RequestID
        body["error"].(gin.H)["trace_id"] = rc.TraceID
    }
    c.JSON(status, body)
}
```

## 8. 字段缺失检测（防御性）

```go
// pkg/middleware/helpers.go
// MustField 在 middleware 入口断言前置 middleware 已写入相应字段。
// 缺失则记录 metric 并 panic（M9 Recover 兜底）。
func MustField(rc *domain.RequestContext, name string, ok bool) {
    if !ok {
        metric.Inc(metric.ContextFieldMissTotal, "field_name", name)
        panic("required domain.RequestContext field missing: " + name)
    }
}
```

使用例（在 M5 入口）：

```go
MustField(rc, "Envelope", rc.Envelope != nil)
```

> 这是临时防御性检查；生产稳定后可改为单元测试覆盖（顺序错误启动即失败）。

## 9. Metric 与告警

| Metric | 类型 | 标签 | 告警 |
|--------|------|------|-----|
| `llm_gateway.middleware.duration_ms` | histogram | `mw_name` | P95 突增 |
| `llm_gateway.middleware.error_total` | counter | `mw_name`, `class` | rate > 阈值 |
| `llm_gateway.context.field_miss_total` | counter | `field_name` | rate > 0 即 P1 告警（顺序异常）|
| `llm_gateway.panic_total` | counter | `component` | rate > 0 即 P0 告警 |
| `llm_gateway.http.request_duration_ms` | histogram | `method`, `path`, `status` | P99 突增 |

详细命名规约 / 告警阈值 / Runbook 见 [07-roadmap](07-roadmap.md) 与未来 `docs/operations/`。

## 10. 测试矩阵

| # | 场景 | 预期 |
|---|------|-----|
| T1 | 正常 Chat 请求 | 10 个 middleware 全跑；rc 字段齐全；HTTP 200 |
| T2 | 缺凭证 | M2 abort 401 |
| T3 | 凭证无效 | M2 abort 401 |
| T4 | body 不可解析 | M3 abort 400 |
| T5 | 模型未注册 | M5 abort 404 |
| T6 | 预算不通过 | M4 abort 403 |
| T7 | 用户层限流 | M6 abort 429 |
| T8 | 模型层限流 | M6 不 abort，M7 RetryExecutor 触发 fallback |
| T9 | endpoint 层限流 | M7 内部 Filter 链直接跳过该 endpoint |
| T10 | 上游 transient 错误 | M7 RetryExecutor L1/L2 重试；最终成功 → 200 |
| T11 | 上游 transient 错误超限 | rc.Error → 502 |
| T12 | 上游 rate limit | RetryExecutor 触发 cooldown + fallback |
| T13 | 中间层 panic | M9 兜底 500，metric 计数 |
| T14 | 内容审核违规 | M8 abort 400 |
| T15 | middleware 顺序错（启动）| `Validate()` 返回 error → 进程不启动 |
| T16 | 字段缺失（运行时）| `field_miss` metric + panic + M9 500 |

## 11. 演进规则

- **新增字段到 `domain.RequestContext`**：必须更新本文档第 2 节；说明哪个 middleware 写入、哪些 middleware 读取
- **新增 middleware**：必须更新本文档第 4 节注册表与第 5 节契约；评估对现有顺序的影响
- **修改 `domain.ErrorClass`**：必须同时更新本文档第 7 节与所有用到的 Adapter
- **新增 domain 包**：必须更新本文档第 1 节包结构图与依赖方向图；确保不引入循环
- **临时性字段**：用 `rc.Extras["xxx"]`，正式化时再升级到 struct 字段
