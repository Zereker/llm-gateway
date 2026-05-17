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
		middleware.Schedule(
			middleware.WithEndpointReader(deps.EndpointReader),
			middleware.WithFallbackCatalog(deps.FallbackCatalog),
			middleware.WithFallbackSubscriptionChecker(deps.FallbackSubscriptionChecker),
			middleware.WithScheduler(deps.Scheduler),
			middleware.WithSender(deps.Sender),
			middleware.WithEndpointRateStore(deps.RateLimitStore),
			middleware.WithMaxAttempts(deps.MaxAttempts),
		),
		middleware.Tracing(
			middleware.WithUsageOutbox(deps.UsageOutbox),
			middleware.WithTracer(deps.AuditTracer),
		),
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
		middleware.Schedule(
			middleware.WithEndpointReader(deps.EndpointReader),
			middleware.WithFallbackCatalog(deps.FallbackCatalog),
			middleware.WithFallbackSubscriptionChecker(deps.FallbackSubscriptionChecker),
			middleware.WithScheduler(deps.Scheduler),
			middleware.WithSender(deps.Sender),
			middleware.WithEndpointRateStore(deps.RateLimitStore),
			middleware.WithMaxAttempts(deps.MaxAttempts),
		),
		middleware.Tracing(
			middleware.WithUsageOutbox(deps.UsageOutbox),
			middleware.WithTracer(deps.AuditTracer),
		),
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
		middleware.Schedule(
			middleware.WithEndpointReader(deps.EndpointReader),
			middleware.WithFallbackCatalog(deps.FallbackCatalog),
			middleware.WithFallbackSubscriptionChecker(deps.FallbackSubscriptionChecker),
			middleware.WithScheduler(deps.Scheduler),
			middleware.WithSender(deps.Sender),
			middleware.WithEndpointRateStore(deps.RateLimitStore),
			middleware.WithMaxAttempts(deps.MaxAttempts),
		),
		middleware.Tracing(
			middleware.WithUsageOutbox(deps.UsageOutbox),
			middleware.WithTracer(deps.AuditTracer),
		),
		noopHandler,
	)
}
