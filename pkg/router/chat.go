package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/middleware"
)

// registerChatRoutes registers the chat modality routes plus their dedicated
// middleware chain.
//
// Paths (each `.POST` carries its own full /v1 prefix, no outer group):
//
//	POST /v1/chat/completions   OpenAI / OpenAI-compat
//	POST /v1/messages           Anthropic
//	POST /v1/responses          OpenAI Responses (added in v1.0; the new protocol's input + instructions shape)
//
// **Protocol tagging**: each path mounts its own WithSourceProtocol before
// Envelope, pinning down "which protocol this path is." Envelope no longer
// does path-based heuristics (DefaultDetector has been removed).
//
// Each modality lists its own required middleware; there's no shared
// buildChain extracted, because modalities are expected to diverge going
// forward (chat adds a Moderator / image adds a multipart Parser / audio adds
// an ASR-only ParamSpec, etc.). As of v0.1 the chains happen to be identical
// across modalities, but they're kept independent in code.
func registerChatRoutes(engine *gin.Engine, deps Deps) {
	// Group the pre-Envelope shared middleware so it doesn't need to be
	// repeated on every POST; the "/" path prefix keeps the group from
	// introducing an extra URL segment.
	chat := engine.Group("/",
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
		middleware.ResponseCache(deps.ResponseCache, deps.CacheTTL),
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
		middleware.ResponseCache(deps.ResponseCache, deps.CacheTTL),
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
		middleware.ResponseCache(deps.ResponseCache, deps.CacheTTL),
		middleware.Schedule(deps.Dispatcher),
		noopHandler,
	)
}
