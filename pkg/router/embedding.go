package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
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
	pre := llmRouteGroup(engine, deps)
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
	registerLLMRoute(pre, deps, routeSpec{
		Path: "/v1/embeddings", Protocol: domain.ProtoOpenAI,
		Modality: domain.ModalityEmbedding, Cache: deps.EmbeddingCache,
	})
}
