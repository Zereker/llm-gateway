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
		middleware.Recover(),
		middleware.Auth(deps.Auth),
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
			middleware.Budget(deps.Budget),
			middleware.ModelService(deps.ModelService),
			middleware.Moderation(deps.Moderation),
			middleware.Limit(deps.Limit),
			middleware.Schedule(deps.Schedule),
			middleware.Tracing(deps.Tracing),
			noopHandler,
		)
	}
}
