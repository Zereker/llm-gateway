package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/middleware"
)

// registerImageRoutes 注册 image 模态路由 + 它专属的 middleware 链。
//
// 路径（每条 `.POST` 自带 /v1 完整前缀）：
//
//	POST /v1/images/generations  OpenAI 文生图
//	POST /v1/images/edits        OpenAI 图编辑（multipart/form-data）
//	POST /v1/images/variations   OpenAI 图变体（multipart/form-data）
//
// v0.1：路由 + middleware 已注册，但没有 image-capable Adapter；
// edits / variations 是 multipart 请求，当前 DefaultParser 只解析 JSON，
// 未来接 image Adapter 时会换 multipart Parser。
func registerImageRoutes(engine *gin.Engine, deps Deps) {
	pre := engine.Group("/",
		middleware.BodyLimit(deps.BodyLimit),
		middleware.Timeout(deps.Timeout),
		middleware.TraceContext(),
		middleware.Recover(),
		middleware.Auth(middleware.WithIdentityProvider(deps.IdentityProvider)),
	)
	for _, p := range []string{"/v1/images/generations", "/v1/images/edits", "/v1/images/variations"} {
		pre.POST(p,
			middleware.WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityImage),
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
}
