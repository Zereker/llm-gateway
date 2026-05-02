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
//   - helpers.go   ops handlers + bodyLimitMW + timeoutMW + noopHandler
package router

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/middleware"
	"github.com/zereker-labs/ai-gateway/pkg/trace"
	"github.com/zereker-labs/ai-gateway/pkg/usage"
)

// Deps 是 NewEngine 的依赖集合。
//
// 各 modality 文件按需取用：
//   - 比如 chat 用 IdentityProvider / Detector / Parser / ModelService / Endpoints / Outbox / Tracer
//   - image 未来可能加 Moderator
//   - audio 未来可能加 multipart Parser
//
// nil 字段：BudgetGate / Moderator nil 时对应 middleware 不注册（v0.1 都默认 NoOp）。
type Deps struct {
	// M2 Auth
	IdentityProvider middleware.IdentityProvider

	// M3 Envelope
	Detector middleware.Detector
	Parser   middleware.Parser

	// M4 Budget (optional)
	BudgetGate middleware.BudgetGate

	// M5 ModelService
	ModelService middleware.ModelServiceProvider

	// M7 Schedule
	Endpoints middleware.EndpointProvider

	// M8 Moderation (optional)
	Moderator middleware.Moderator

	// M10 Tracing
	Outbox usage.OutboxPublisher
	Tracer trace.Tracer

	// Pre-middleware
	BodyLimit int64         // 0 = 不限制
	Timeout   time.Duration // 0 = 不限超时
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
