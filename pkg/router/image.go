package router

import "github.com/gin-gonic/gin"

// registerImageRoutes registers image-modality endpoints + middleware chain.
//
// 路径：
//   POST /v1/images/generations  OpenAI 文生图
//   POST /v1/images/edits        OpenAI 图编辑（multipart/form-data）
//   POST /v1/images/variations   OpenAI 图变体（multipart/form-data）
//
// v0.1：路由 + middleware 已注册，但没有 image-capable Adapter；
// 实际请求会在 M5 / M7 失败。
//
// 注：edits / variations 是 multipart 请求；M3 DefaultParser 当前只解析 JSON，
// 等接入 image Adapter 时一并扩展 Parser 支持 multipart。
func registerImageRoutes(api *gin.RouterGroup, deps Deps) {
	image := api.Group("/", buildChain(deps)...)
	image.POST("/images/generations", noopHandler)
	image.POST("/images/edits", noopHandler)
	image.POST("/images/variations", noopHandler)
}
