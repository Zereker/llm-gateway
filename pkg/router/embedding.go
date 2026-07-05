package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/middleware"
)

// registerEmbeddingRoutes registers the embedding modality routes plus their
// dedicated middleware chain.
//
// Paths (each `.POST` carries its own full /v1 prefix):
//
//	POST /v1/embeddings  OpenAI / OpenAI-compat
//
// The OpenAI Adapter's Metadata.SupportedModalities already includes
// ModalityEmbedding, so it works as soon as an embedding model + endpoint is
// configured.
func registerEmbeddingRoutes(engine *gin.Engine, deps Deps) {
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
		// Embeddings are inherently deterministic (no sampling parameters) —
		// ResponseCache caches them by default, and a hit returns the vector
		// directly, skipping the upstream. See the embeddings exception in
		// middleware.ResponseCache.
		//
		// **Dedicated EmbeddingCache (exact match), not reusing deps.Cache**:
		// when the global config uses semantic cache, deps.Cache is a
		// SemanticCache, and an embedding request's input would get extracted
		// by extractPrompt for similarity matching — a semantic hit is wrong
		// for embeddings (paraphrases have different correct vectors).
		// Embeddings must use exact-match caching.
		deps.EmbeddingCache,
		middleware.Schedule(deps.Dispatcher),
		noopHandler,
	)
}
