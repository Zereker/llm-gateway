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
		bodyLimitMW(deps.BodyLimit),
		timeoutMW(deps.Timeout),
		middleware.TraceContext(),
		middleware.Recover(),
		middleware.Auth(middleware.AuthDeps{Provider: deps.IdentityProvider}),
		middleware.Envelope(middleware.EnvelopeDeps{Detector: deps.Detector, Parser: deps.Parser}),
		middleware.ModelService(middleware.ModelServiceDeps{Provider: deps.ModelService}),
		middleware.Schedule(middleware.ScheduleDeps{Endpoints: deps.Endpoints}),
		middleware.Tracing(middleware.TracingDeps{Outbox: deps.Outbox, Tracer: deps.Tracer}),
	)
	embed.POST("/embeddings", noopHandler)
}
