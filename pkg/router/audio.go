package router

import "github.com/gin-gonic/gin"

// registerAudioRoutes registers audio-modality endpoints (TTS + ASR) + middleware chain.
//
// 路径：
//   POST /v1/audio/speech          TTS：文本 → 音频
//   POST /v1/audio/transcriptions  ASR：音频 → 文本（同语言，multipart）
//   POST /v1/audio/translations    ASR：音频 → 英文文本（multipart）
//
// v0.1：路由已注册，但没有 audio-capable Adapter。
// transcriptions / translations 是 multipart 请求；同 image 注释。
func registerAudioRoutes(api *gin.RouterGroup, deps Deps) {
	audio := api.Group("/", buildChain(deps)...)
	audio.POST("/audio/speech", noopHandler)
	audio.POST("/audio/transcriptions", noopHandler)
	audio.POST("/audio/translations", noopHandler)
}
