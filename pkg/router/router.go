// Package router 装配 gin.Engine：注册 middleware 链 + 按模态拆分的 LLM 路由 + 操作端点。
//
// 这是 v0.1 的"默认装配"：固定的 M1-M10 顺序、固定的路径规则。
// 高级用户可以不用本包、自己 import pkg/middleware 直接装配，
// 以获得完全自定义的中间件顺序 / 路由 / 多协议前缀等灵活性。
//
// 文件按模态分：
//   - chat.go     /v1/chat/completions, /v1/messages
//   - image.go    /v1/images/{generations,edits,variations}
//   - audio.go    /v1/audio/{speech,transcriptions,translations}（TTS + ASR）
//   - embedding.go /v1/embeddings
//   - helpers.go  共享 middleware（bodyLimit / timeout）+ ops handlers
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
// nil 字段会按"该 middleware 没有有效依赖"处理：
//   - BudgetGate     nil → 不注册 M4（等价 AlwaysPass）
//   - Moderator      nil → 不注册 M8（等价 NoOp）
//   - Outbox / Tracer nil → M10 仍注册但相关动作 NoOp
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

// NewEngine 构造 gin.Engine 并完成全部装配：
//
//   - 操作端点（顶层，不走主 middleware 链）：
//     GET /healthz, /readyz, /metrics
//   - LLM API：在 /v1 路由组内，按模态拆分注册（详见各 modality 文件）；
//     每个模态自己调 buildChain(deps) 注册 middleware，可独立定制
//
// 返回的 *gin.Engine 可直接 srv.ListenAndServe() 或在测试里 ServeHTTP。
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

// buildChain 按固定顺序装配 v0.1 的 middleware：
//
//	bodyLimit → timeout → M1 TraceContext → M9 Recover →
//	M2 Auth → M3 Envelope → [M4 Budget?] → M5 ModelService →
//	[M8 Moderation?] → M7 Schedule → M10 Tracing
//
// M4 / M8 仅在对应依赖非 nil 时（v0.5+ 加 handler 后）注册。
// M6 Limit 在 v0.1 不注册，等 v0.5+ 加 ratelimit.Checker 实现。
func buildChain(deps Deps) []gin.HandlerFunc {
	var hs []gin.HandlerFunc

	if deps.BodyLimit > 0 {
		hs = append(hs, bodyLimitMW(deps.BodyLimit))
	}
	if deps.Timeout > 0 {
		hs = append(hs, timeoutMW(deps.Timeout))
	}

	hs = append(hs,
		middleware.TraceContext(),
		middleware.Recover(),
		middleware.Auth(middleware.AuthDeps{Provider: deps.IdentityProvider}),
		middleware.Envelope(middleware.EnvelopeDeps{Detector: deps.Detector, Parser: deps.Parser}),
	)

	// M4 Budget / M8 Moderation handler 实现待 v0.5+；当前只保留 dep 字段供未来注册
	_ = deps.BudgetGate
	_ = deps.Moderator

	hs = append(hs,
		middleware.ModelService(middleware.ModelServiceDeps{Provider: deps.ModelService}),
		// M6 Limit skipped
		middleware.Schedule(middleware.ScheduleDeps{Endpoints: deps.Endpoints}),
		middleware.Tracing(middleware.TracingDeps{Outbox: deps.Outbox, Tracer: deps.Tracer}),
	)

	return hs
}
