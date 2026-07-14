package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/middleware"
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
	moderationOptions := []middleware.ModerationOption{
		middleware.WithModerator(deps.Moderator),
		middleware.WithPolicyAuditTracer(deps.AuditTracer),
		middleware.WithPolicyResolver(deps.PolicyResolver),
	}
	if deps.PolicyEngine != nil {
		moderationOptions = append(moderationOptions, middleware.WithPolicyEngine(deps.PolicyEngine))
	}

	handlers := []gin.HandlerFunc{
		middleware.WithSourceProtocol(spec.Protocol, spec.Modality),
		middleware.Envelope(deps.Handlers),
		middleware.Budget(middleware.WithBudgetGate(deps.BudgetGate)),
		middleware.ModelService(
			middleware.WithModelCatalog(deps.ModelCatalog),
			middleware.WithSubscriptionChecker(deps.SubscriptionChecker),
			middleware.WithVirtualModelResolver(deps.VirtualModelResolver),
		),
		// M6 before M8: rate limiting is a cheap Redis reserve, moderation is
		// an external HTTP call — a request that is going to be 429-rejected
		// must not spend a moderation call first (cost + provider pressure
		// amplification under abusive traffic). M6 needs rc.ModelService for
		// its per_model buckets, so it cannot move any earlier than this.
		middleware.Limit(
			middleware.WithLimitStore(deps.RateLimitStore),
			middleware.WithLimitPolicies(deps.QuotaPolicies),
		),
		middleware.Moderation(moderationOptions...),
	}
	if spec.Cache != nil {
		handlers = append(handlers, spec.Cache)
	}

	// Schedule is the terminal handler at the center of the middleware onion.
	// It writes the response itself; outer middleware resumes on its return.
	handlers = append(handlers, middleware.Schedule(deps.Dispatcher))
	group.POST(spec.Path, handlers...)
}
