package router

import (
	"github.com/gin-gonic/gin"

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
	embed := api.Group("/",
		middleware.BodyLimit(deps.BodyLimit),
		middleware.Timeout(deps.Timeout),
		middleware.TraceContext(),
		middleware.Recover(),
		middleware.Auth(deps.Auth),
		middleware.Envelope(deps.Envelope),
		middleware.ModelService(deps.ModelService),
		middleware.Schedule(deps.Schedule),
		middleware.Tracing(deps.Tracing),
	)
	embed.POST("/embeddings", noopHandler)
}
