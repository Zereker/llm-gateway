// Package router 装配 gin.Engine：注册按模态拆分的 LLM 路由 + 操作端点。
package router

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/middleware"
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
	RateLimitStore middleware.RateLimitStore
	QuotaPolicies  middleware.QuotaPolicies

	// M7 Schedule
	//
	// fallback model 解析 + catalog/subscription 校验在 M5 完成，结果走 rc.ModelChain；
	// M7 这里不再需要 catalog / subscriptions 依赖。
	EndpointReader middleware.EndpointReader
	Scheduler      middleware.Scheduler
	Sender         middleware.Sender
	MaxAttempts    int
	// EndpointRateStore 不另开字段——M7 复用 RateLimitStore（endpoint 桶 key 跟 user 桶 key
	// 在同一存储里）。

	// M8 Moderation
	Moderator middleware.Moderator

	// M10 Tracing
	UsageOutbox UsageOutbox
	AuditTracer AuditTracer
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

	registerOpsRoutes(engine)
	registerChatRoutes(engine, deps)
	registerImageRoutes(engine, deps)
	registerAudioRoutes(engine, deps)
	registerEmbeddingRoutes(engine, deps)

	return engine
}
