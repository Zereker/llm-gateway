package router

import "github.com/gin-gonic/gin"

// registerEmbeddingRoutes registers embedding endpoints + middleware chain.
//
// 路径：
//   POST /v1/embeddings  OpenAI / OpenAI-compat
//
// OpenAI Adapter 的 Metadata.SupportedModalities 已含 ModalityEmbedding，
// 配一个 embedding model + endpoint 就能用。
func registerEmbeddingRoutes(api *gin.RouterGroup, deps Deps) {
	embed := api.Group("/", buildChain(deps)...)
	embed.POST("/embeddings", noopHandler)
}
