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
		middleware.Auth(deps.Auth),
	)
	pre.POST("/v1/embeddings",
		middleware.WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityEmbedding),
		middleware.Envelope(),
		middleware.Budget(deps.Budget),
		middleware.ModelService(deps.ModelService),
		middleware.Limit(deps.Limit),
		middleware.Moderation(deps.Moderation),
		middleware.Schedule(deps.Schedule),
		middleware.Tracing(deps.Tracing),
		noopHandler,
	)
}
