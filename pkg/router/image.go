package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/middleware"
)

// registerImageRoutes 注册 image 模态路由 + 它专属的 middleware 链。
//
// 路径：
//   POST /v1/images/generations  OpenAI 文生图
//   POST /v1/images/edits        OpenAI 图编辑（multipart/form-data）
//   POST /v1/images/variations   OpenAI 图变体（multipart/form-data）
//
// v0.1：路由 + middleware 已注册，但没有 image-capable Adapter；
// edits / variations 是 multipart 请求，当前 DefaultParser 只解析 JSON，
// 未来接 image Adapter 时会换 multipart Parser。
func registerImageRoutes(api *gin.RouterGroup, deps Deps) {
	image := api.Group("/",
		middleware.BodyLimit(deps.BodyLimit),
		middleware.Timeout(deps.Timeout),
		middleware.TraceContext(),
		middleware.Recover(),
		middleware.Auth(middleware.AuthDeps{Provider: deps.IdentityProvider}),
		middleware.Envelope(middleware.EnvelopeDeps{Detector: deps.Detector, Parser: deps.Parser}),
		middleware.ModelService(middleware.ModelServiceDeps{Provider: deps.ModelService}),
		middleware.Schedule(middleware.ScheduleDeps{Endpoints: deps.Endpoints}),
		middleware.Tracing(middleware.TracingDeps{Outbox: deps.Outbox, Tracer: deps.Tracer}),
	)
	image.POST("/images/generations", noopHandler)
	image.POST("/images/edits", noopHandler)
	image.POST("/images/variations", noopHandler)
}
