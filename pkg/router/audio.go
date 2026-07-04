package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/middleware"
)

// registerAudioRoutes 注册 audio 模态路由（TTS + ASR）+ 它专属的 middleware 链。
//
// 路径（每条 `.POST` 自带 /v1 完整前缀）：
//
//	POST /v1/audio/speech          TTS：文本 → 音频
//	POST /v1/audio/transcriptions  ASR：音频 → 文本（同语言，multipart）
//	POST /v1/audio/translations    ASR：音频 → 英文文本（multipart）
//
// v0.1：路由已注册，但没有 audio-capable Adapter；transcriptions /
// translations 是 multipart 请求，同 image。
func registerAudioRoutes(engine *gin.Engine, deps Deps) {
	pre := engine.Group("/",
		middleware.BodyLimit(deps.BodyLimit),
		middleware.Timeout(deps.Timeout),
		middleware.TraceContext(),
		// M10 Tracing 挂在 Recover **外层**（post-c.Next() 收尾）：
		//   - 任何后续 middleware abort（401/429/503）→ 洋葱返程仍执行收尾
		//     ——请求 metric / usage 事件 / decision 审计没有盲区
		//   - panic → 内层 Recover 先恢复并写 500，控制流正常返回，收尾照跑
		//     且看到的是最终 500 状态
		// （旧版挂链尾，abort 一律跳过 → 429 风暴在 M10 指标里隐身）
		middleware.Tracing(
			middleware.WithUsageOutbox(deps.UsageOutbox),
			middleware.WithTracer(deps.AuditTracer),
		),
		middleware.Recover(),
		middleware.Auth(middleware.WithIdentityProvider(deps.IdentityProvider)),
	)

	routes := []struct {
		path string
		mod  domain.Modality
	}{
		{"/v1/audio/speech", domain.ModalityTTS},
		{"/v1/audio/transcriptions", domain.ModalityASR},
		{"/v1/audio/translations", domain.ModalityASR},
	}
	for _, r := range routes {
		pre.POST(r.path,
			middleware.WithSourceProtocol(domain.ProtoOpenAI, r.mod),
			middleware.Envelope(),
			middleware.Budget(middleware.WithBudgetGate(deps.BudgetGate)),
			middleware.ModelService(
				middleware.WithModelCatalog(deps.ModelCatalog),
				middleware.WithSubscriptionChecker(deps.SubscriptionChecker),
			),
			middleware.Moderation(middleware.WithModerator(deps.Moderator)),
			middleware.Limit(
				middleware.WithLimitStore(deps.RateLimitStore),
				middleware.WithLimitPolicies(deps.QuotaPolicies),
			),
			middleware.Schedule(deps.Dispatcher),
			noopHandler,
		)
	}
}
