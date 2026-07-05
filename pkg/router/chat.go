package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/middleware"
)

// registerChatRoutes 注册 chat 模态路由 + 它专属的 middleware 链。
//
// 路径（每条 `.POST` 自带 /v1 完整前缀，不依赖外层 group）：
//
//	POST /v1/chat/completions   OpenAI / OpenAI-compat
//	POST /v1/messages           Anthropic
//	POST /v1/responses          OpenAI Responses（v1.0 加；新协议 input + instructions shape）
//
// **协议打标**：每条路径在 Envelope 之前各自挂一个 WithSourceProtocol，把
// "这个 path 是哪个协议" 钉死。Envelope 不再做 path 启发式（DefaultDetector 已删）。
//
// 每个模态自己列出需要的 middleware；不抽公共 buildChain，因为不同模态
// 未来会差异化（chat 加 Moderator / image 加 multipart Parser / audio 加
// ASR-only ParamSpec 等）。当前 v0.1 各模态链恰好一致，但代码上独立。
func registerChatRoutes(engine *gin.Engine, deps Deps) {
	// pre-Envelope 共享中间件做成一个 group，省得每条 POST 都重复列；
	// 路径前缀 "/" 让 group 不引入额外 URL 段。
	chat := engine.Group("/",
		middleware.BodyLimit(deps.BodyLimit),
		middleware.Timeout(deps.Timeout),
		middleware.TraceContext(),
		// M10 Tracing 挂在 Recover **外层**（post-c.Next() 收尾）：
		//   - 任何后续 middleware abort（401/429/503）→ 洋葱返程仍执行收尾
		//     ——请求 metric / usage 事件 / decision 审计没有盲区
		//   - panic → 内层 Recover 先恢复并写 500，控制流正常返回，收尾照跑
		//     且看到的是最终 500 状态
		// （旧版挂链尾，abort 一律跳过 → 429 风暴在 M10 指标里隐身）
		middleware.Tracing(
			middleware.WithUsageOutbox(deps.UsageOutbox),
			middleware.WithTracer(deps.AuditTracer),
		),
		middleware.Recover(),
		middleware.Auth(middleware.WithIdentityProvider(deps.IdentityProvider)),
	)

	chat.POST("/v1/chat/completions",
		middleware.WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityChat),
		middleware.Envelope(),
		middleware.Budget(middleware.WithBudgetGate(deps.BudgetGate)),
		middleware.ModelService(
			middleware.WithModelCatalog(deps.ModelCatalog),
			middleware.WithSubscriptionChecker(deps.SubscriptionChecker),
		),
		middleware.Moderation(middleware.WithModerator(deps.Moderator)),
		middleware.Limit(
			middleware.WithLimitStore(deps.RateLimitStore),
			middleware.WithLimitPolicies(deps.QuotaPolicies),
		),
		deps.Cache,
		middleware.Schedule(deps.Dispatcher),
		noopHandler,
	)

	chat.POST("/v1/messages",
		middleware.WithSourceProtocol(domain.ProtoAnthropic, domain.ModalityChat),
		middleware.Envelope(),
		middleware.Budget(middleware.WithBudgetGate(deps.BudgetGate)),
		middleware.ModelService(
			middleware.WithModelCatalog(deps.ModelCatalog),
			middleware.WithSubscriptionChecker(deps.SubscriptionChecker),
		),
		middleware.Moderation(middleware.WithModerator(deps.Moderator)),
		middleware.Limit(
			middleware.WithLimitStore(deps.RateLimitStore),
			middleware.WithLimitPolicies(deps.QuotaPolicies),
		),
		deps.Cache,
		middleware.Schedule(deps.Dispatcher),
		noopHandler,
	)

	chat.POST("/v1/responses",
		middleware.WithSourceProtocol(domain.ProtoResponses, domain.ModalityChat),
		middleware.Envelope(),
		middleware.Budget(middleware.WithBudgetGate(deps.BudgetGate)),
		middleware.ModelService(
			middleware.WithModelCatalog(deps.ModelCatalog),
			middleware.WithSubscriptionChecker(deps.SubscriptionChecker),
		),
		middleware.Moderation(middleware.WithModerator(deps.Moderator)),
		middleware.Limit(
			middleware.WithLimitStore(deps.RateLimitStore),
			middleware.WithLimitPolicies(deps.QuotaPolicies),
		),
		deps.Cache,
		middleware.Schedule(deps.Dispatcher),
		noopHandler,
	)
}
