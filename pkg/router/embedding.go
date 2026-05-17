package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/middleware"
)

// registerEmbeddingRoutes 注册 embedding 模态路由 + 它专属的 middleware 链。
//
// 路径（每条 `.POST` 自带 /v1 完整前缀）：
//
//	POST /v1/embeddings  OpenAI / OpenAI-compat
//
// OpenAI Adapter 的 Metadata.SupportedModalities 已含 ModalityEmbedding，
// 配一个 embedding model + endpoint 就能用。
func registerEmbeddingRoutes(engine *gin.Engine, deps Deps) {
	pre := engine.Group("/",
		middleware.BodyLimit(deps.BodyLimit),
		middleware.Timeout(deps.Timeout),
		middleware.TraceContext(),
		middleware.Recover(),
		middleware.Auth(middleware.WithIdentityProvider(deps.IdentityProvider)),
	)
	pre.POST("/v1/embeddings",
		middleware.WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityEmbedding),
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
