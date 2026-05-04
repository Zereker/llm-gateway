// Package router 装配 gin.Engine：注册按模态拆分的 LLM 路由 + 操作端点。
//
// 这是 v0.1 的"默认装配"。高级用户可不用本包、自己 import pkg/middleware
// 直接装配以获得完全自定义的路由 / 中间件顺序。
//
// 文件按模态分；每个模态文件**自己列出**它需要的 middleware（不抽公共 helper），
// 这样 chat / image / audio / embedding 各自独立演进，无共享代码绑定。
//
//   - chat.go      /v1/chat/completions, /v1/messages
//   - image.go     /v1/images/{generations,edits,variations}
//   - audio.go     /v1/audio/{speech,transcriptions,translations}（TTS + ASR）
//   - embedding.go /v1/embeddings
//   - helpers.go   ops handlers + noopHandler
//
// **客户端协议范围**：网关只暴露 OpenAI Chat / Anthropic Messages / OpenAI Responses
// 三种文本协议（Responses 留 v1.0）。Gemini 作为**上游**支持（pkg/adapter/gemini +
// pkg/translator/openai_gemini），不暴露 Gemini 客户端入口——客户端用 OpenAI SDK
// 调网关，网关帮翻译到 Gemini 上游。
package router

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/middleware"
)

// Deps 是 NewEngine 的依赖集合，按 middleware 切分。
//
// 每个子字段就是对应 middleware 的 Deps，调用形态零拆装：
//
//	middleware.Auth(deps.Auth)
//	middleware.Envelope(deps.Envelope)
//
// 加新 middleware → 加一个新子字段；老调用不动。
//
// BodyLimit / Timeout 是 pre-middleware 的标量参数，不走 Deps 结构。
type Deps struct {
	// Pre-middleware
	BodyLimit int64         // 0 = 不限制
	Timeout   time.Duration // 0 = 不限超时

	// Middleware deps（按 M-编号顺序）
	Auth         middleware.AuthDeps         // M2
	Envelope     middleware.EnvelopeDeps     // M3
	Budget       middleware.BudgetDeps       // M4
	ModelService middleware.ModelServiceDeps // M5
	Limit        middleware.LimitDeps        // M6
	Moderation   middleware.ModerationDeps   // M8
	Schedule     middleware.ScheduleDeps     // M7
	Tracing      middleware.TracingDeps      // M10
}

// NewEngine 构造 gin.Engine 并完成全部装配。
func NewEngine(deps Deps) *gin.Engine {
	if gin.Mode() == gin.DebugMode {
		gin.SetMode(gin.ReleaseMode)
	}
	engine := gin.New()

	registerOpsRoutes(engine)

	api := engine.Group("/v1") // 不在此处 attach middleware；交给各 modality 文件
	registerChatRoutes(api, deps)
	registerImageRoutes(api, deps)
	registerAudioRoutes(api, deps)
	registerEmbeddingRoutes(api, deps)

	return engine
}
