package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/middleware"
)

// registerChatRoutes 注册 chat 模态路由 + 它专属的 middleware 链。
//
// 路径：
//   POST /v1/chat/completions   OpenAI / OpenAI-compat
//   POST /v1/messages           Anthropic（v0.5+ 加 Anthropic adapter 后生效）
//
// 每个模态自己列出需要的 middleware；不抽公共 buildChain，因为不同模态
// 未来会差异化（chat 加 Moderator / image 加 multipart Parser / audio 加
// ASR-only ParamSpec 等）。当前 v0.1 各模态链恰好一致，但代码上独立。
func registerChatRoutes(api *gin.RouterGroup, deps Deps) {
	chat := api.Group("/",
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
	chat.POST("/chat/completions", noopHandler)
	chat.POST("/messages", noopHandler)
}
