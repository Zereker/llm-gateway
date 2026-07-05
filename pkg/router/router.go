// Package router 装配 gin.Engine：注册按模态拆分的 LLM 路由 + 操作端点。
package router

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/middleware"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
)

// Deps 是 NewEngine 的依赖集合：直接持有 middleware 各 port 的实现引用 +
// 几个 pre-middleware 标量参数。
//
// **不再用 `[]middleware.XxxOption` 包装**——那层 wrapping 把 middleware 内部
// 装配语法 leak 到了 router 的公开接口，让 caller / 测试都得写
// `middleware.WithFoo(stubFoo)` 才能传一个 stub。现在 Deps 字段就是 port 类型
// 本身，caller 直接传 stub / impl，router 内部各模态文件再 wrap 成 With* 选项
// 喂给 middleware factory。
//
// 添加 middleware 可选项时：扩 Deps 加一个 port 类型字段；各模态文件按需 wrap。
type Deps struct {
	// BodyLimit / Timeout 是 pre-middleware 标量参数（per-request HTTP 默认值）。
	BodyLimit int64
	Timeout   time.Duration

	// M2 Auth
	IdentityProvider middleware.IdentityProvider

	// M4 Budget
	BudgetGate middleware.BudgetGate

	// M5 ModelService
	ModelCatalog        middleware.ModelCatalog
	SubscriptionChecker middleware.SubscriptionChecker

	// M6 Limit（user 侧 RPM/RPS + TPM）
	RateLimitStore ratelimit.Store
	QuotaPolicies  middleware.QuotaPolicies

	// M7 Schedule
	//
	// Dispatcher 是 Selector + Invoker + Policy 的协调器（pkg/dispatch）。
	// M7 middleware 现在是 thin adapter，调 Dispatcher.Dispatch 完事——
	// fallback model / retry / streaming 全部在 dispatch 内编排，router 不再认
	// EndpointReader / Scheduler / Sender / MaxAttempts 等细节。
	Dispatcher *dispatch.Dispatcher

	// 响应缓存（M6 之后、M7 之前）；nil = 不缓存（中间件 no-op）。
	ResponseCache middleware.ResponseCacheStore
	CacheTTL      time.Duration

	// M8 Moderation
	Moderator middleware.Moderator

	// M10 Tracing
	UsageOutbox UsageOutbox
	AuditTracer AuditTracer

	// Readiness /readyz 的依赖检查项（SQL ping / Redis ping）；空 = 静态 200。
	// cmd 装配时注入，检查逻辑见 helpers.go readyzHandler。
	Readiness []ReadinessChecker
}

// UsageOutbox / AuditTracer 类型别名让 Deps 字段类型描述跟 middleware port 名一致，
// 同时给 router 测试桩一个清晰的目标。
type (
	UsageOutbox = middleware.UsageOutbox
	AuditTracer = middleware.AuditTracer
)

// NewEngine 构造 gin.Engine 并完成全部装配。
func NewEngine(deps Deps) *gin.Engine {
	engine := gin.New()

	// 兜底 recovery：M9 Recover 挂在 M1 之后，BodyLimit / Timeout（M1 之前）的
	// panic 覆盖不到——没有这层时那类 panic 只有 net/http 的连接级 recover，
	// 客户端看到的是连接重置而不是 500。gin.Recovery 的 500 没有我们的 JSON
	// 错误结构，但它只是最后防线，正常路径永远走不到。
	engine.Use(gin.Recovery())

	registerOpsRoutes(engine, deps.Readiness)
	registerChatRoutes(engine, deps)
	registerImageRoutes(engine, deps)
	registerAudioRoutes(engine, deps)
	registerEmbeddingRoutes(engine, deps)

	return engine
}
