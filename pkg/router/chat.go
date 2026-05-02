package router

import "github.com/gin-gonic/gin"

// registerChatRoutes registers chat completion endpoints + middleware chain.
//
// 路径：
//   POST /v1/chat/completions   OpenAI / OpenAI-compat
//   POST /v1/messages           Anthropic（v0.5+ 加 Anthropic adapter 后生效）
//
// 在本文件内显式装配 middleware（buildChain），未来可独立调整：
// 例如给 chat 加 Moderator，或在 chat 链里调用一个 chat-specific 的 ParamSpec preprocessor。
func registerChatRoutes(api *gin.RouterGroup, deps Deps) {
	chat := api.Group("/", buildChain(deps)...)
	chat.POST("/chat/completions", noopHandler)
	chat.POST("/messages", noopHandler)
}
