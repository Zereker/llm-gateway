package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/middleware"
)

// routeSpec describes only the dimensions in which an LLM route differs.
// The security, quota and observability order is centralized below.
type routeSpec struct {
	Path     string
	Protocol domain.Protocol
	Modality domain.Modality
	Cache    gin.HandlerFunc
}

func llmRouteGroup(engine *gin.Engine, deps Deps) *gin.RouterGroup {
	return engine.Group("/",
		middleware.BodyLimit(deps.BodyLimit),
		middleware.Timeout(deps.Timeout),
		middleware.TraceContext(),
		middleware.Tracing(
			middleware.WithUsageOutbox(deps.UsageOutbox),
			middleware.WithTracer(deps.AuditTracer),
		),
		middleware.Recover(),
		middleware.Auth(middleware.WithIdentityProvider(deps.IdentityProvider)),
	)
}

func registerLLMRoute(group *gin.RouterGroup, deps Deps, spec routeSpec) {
	handlers := []gin.HandlerFunc{
		middleware.WithSourceProtocol(spec.Protocol, spec.Modality),
		middleware.Envelope(deps.Handlers),
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
	}
	if spec.Cache != nil {
		handlers = append(handlers, spec.Cache)
	}
	handlers = append(handlers, middleware.Schedule(deps.Dispatcher), noopHandler)
	group.POST(spec.Path, handlers...)
}
