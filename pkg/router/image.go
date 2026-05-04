package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/middleware"
)

// registerImageRoutes 注册 image 模态路由 + 它专属的 middleware 链。
//
// 路径（每条 `.POST` 自带 /v1 完整前缀）：
//
//	POST /v1/images/generations  OpenAI 文生图
//	POST /v1/images/edits        OpenAI 图编辑（multipart/form-data）
//	POST /v1/images/variations   OpenAI 图变体（multipart/form-data）
//
// v0.1：路由 + middleware 已注册，但没有 image-capable Adapter；
// edits / variations 是 multipart 请求，当前 DefaultParser 只解析 JSON，
// 未来接 image Adapter 时会换 multipart Parser。
func registerImageRoutes(engine *gin.Engine, deps Deps) {
	pre := engine.Group("/",
		middleware.BodyLimit(deps.BodyLimit),
		middleware.Timeout(deps.Timeout),
		middleware.TraceContext(),
		middleware.Recover(),
		middleware.Auth(deps.Auth),
	)
	for _, p := range []string{"/v1/images/generations", "/v1/images/edits", "/v1/images/variations"} {
		pre.POST(p,
			middleware.WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityImage),
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
}
