package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/middleware"
)

// registerAudioRoutes 注册 audio 模态路由（TTS + ASR）+ 它专属的 middleware 链。
//
// 路径：
//   POST /v1/audio/speech          TTS：文本 → 音频
//   POST /v1/audio/transcriptions  ASR：音频 → 文本（同语言，multipart）
//   POST /v1/audio/translations    ASR：音频 → 英文文本（multipart）
//
// v0.1：路由已注册，但没有 audio-capable Adapter；transcriptions /
// translations 是 multipart 请求，同 image。
func registerAudioRoutes(api *gin.RouterGroup, deps Deps) {
	pre := api.Group("/",
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
		{"/audio/speech", domain.ModalityTTS},
		{"/audio/transcriptions", domain.ModalityASR},
		{"/audio/translations", domain.ModalityASR},
	}
	for _, r := range routes {
		pre.POST(r.path,
			middleware.WithSourceProtocol(domain.ProtoOpenAI, r.mod),
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
}
