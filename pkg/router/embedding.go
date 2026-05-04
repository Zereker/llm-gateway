package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/middleware"
)

// registerEmbeddingRoutes 注册 embedding 模态路由 + 它专属的 middleware 链。
//
// 路径：
//   POST /v1/embeddings  OpenAI / OpenAI-compat
//
// OpenAI Adapter 的 Metadata.SupportedModalities 已含 ModalityEmbedding，
// 配一个 embedding model + endpoint 就能用。
func registerEmbeddingRoutes(api *gin.RouterGroup, deps Deps) {
	pre := api.Group("/",
		middleware.BodyLimit(deps.BodyLimit),
		middleware.Timeout(deps.Timeout),
		middleware.TraceContext(),
		middleware.Recover(),
		middleware.Auth(deps.Auth),
	)
	pre.POST("/embeddings",
		middleware.WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityEmbedding),
		middleware.Envelope(deps.Envelope),
		middleware.Budget(deps.Budget),
		middleware.ModelService(deps.ModelService),
		middleware.Limit(deps.Limit),
		middleware.Moderation(deps.Moderation),
		middleware.Schedule(deps.Schedule),
		middleware.Tracing(deps.Tracing),
		noopHandler,
	)
}
