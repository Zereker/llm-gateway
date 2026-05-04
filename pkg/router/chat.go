package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/middleware"
)

// registerChatRoutes 注册 chat 模态路由 + 它专属的 middleware 链。
//
// 路径：
//   POST /v1/chat/completions   OpenAI / OpenAI-compat
//   POST /v1/messages           Anthropic
//   POST /v1/responses          OpenAI Responses（v1.0 加；新协议 input + instructions shape）
//
// **协议打标**：每条路径在 Envelope 之前各自挂一个 WithSourceProtocol，把
// "这个 path 是哪个协议" 钉死。Envelope 不再做 path 启发式（DefaultDetector 已删）。
//
// 每个模态自己列出需要的 middleware；不抽公共 buildChain，因为不同模态
// 未来会差异化（chat 加 Moderator / image 加 multipart Parser / audio 加
// ASR-only ParamSpec 等）。当前 v0.1 各模态链恰好一致，但代码上独立。
func registerChatRoutes(api *gin.RouterGroup, deps Deps) {
	pre := api.Group("/",
		middleware.BodyLimit(deps.BodyLimit),
		middleware.Timeout(deps.Timeout),
		middleware.TraceContext(),
		middleware.Recover(),
		middleware.Auth(deps.Auth),
	)

	pre.POST("/chat/completions",
		middleware.WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityChat),
		middleware.Envelope(deps.Envelope),
		middleware.Budget(deps.Budget),
		middleware.ModelService(deps.ModelService),
		middleware.Limit(deps.Limit),
		middleware.Moderation(deps.Moderation),
		middleware.Schedule(deps.Schedule),
		middleware.Tracing(deps.Tracing),
		noopHandler,
	)

	pre.POST("/messages",
		middleware.WithSourceProtocol(domain.ProtoAnthropic, domain.ModalityChat),
		middleware.Envelope(deps.Envelope),
		middleware.Budget(deps.Budget),
		middleware.ModelService(deps.ModelService),
		middleware.Limit(deps.Limit),
		middleware.Moderation(deps.Moderation),
		middleware.Schedule(deps.Schedule),
		middleware.Tracing(deps.Tracing),
		noopHandler,
	)

	pre.POST("/responses",
		middleware.WithSourceProtocol(domain.ProtoResponses, domain.ModalityChat),
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
