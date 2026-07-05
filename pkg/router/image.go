package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/middleware"
)

// registerImageRoutes registers the image modality routes plus their
// dedicated middleware chain.
//
// Paths (each `.POST` carries its own full /v1 prefix):
//
//	POST /v1/images/generations  OpenAI text-to-image
//	POST /v1/images/edits        OpenAI image edits (multipart/form-data)
//	POST /v1/images/variations   OpenAI image variations (multipart/form-data)
//
// v0.1: the routes + middleware are registered, but there's no image-capable
// Adapter yet; edits / variations are multipart requests, but the current
// DefaultParser only parses JSON — a multipart Parser will replace it once an
// image Adapter is wired in.
func registerImageRoutes(engine *gin.Engine, deps Deps) {
	pre := engine.Group("/",
		middleware.BodyLimit(deps.BodyLimit),
		middleware.Timeout(deps.Timeout),
		middleware.TraceContext(),
		// M10 Tracing is mounted **outside** Recover (its finishing logic runs
		// post-c.Next()):
		//   - any subsequent middleware abort (401/429/503) -> the onion's
		//     return leg still runs the finishing logic, so request metrics /
		//     usage events / decision audit have no blind spot
		//   - panic -> the inner Recover recovers first and writes 500,
		//     control flow returns normally, finishing logic still runs and
		//     sees the final 500 status
		// (the old version was mounted at the end of the chain, so any abort
		// was skipped entirely -> 429 storms went invisible in M10 metrics)
		middleware.Tracing(
			middleware.WithUsageOutbox(deps.UsageOutbox),
			middleware.WithTracer(deps.AuditTracer),
		),
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
			middleware.Schedule(deps.Dispatcher),
			noopHandler,
		)
	}
}
