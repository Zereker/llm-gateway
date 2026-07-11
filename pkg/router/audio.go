package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// registerAudioRoutes registers the audio modality routes (TTS + ASR) plus
// their dedicated middleware chain.
//
// Paths (each `.POST` carries its own full /v1 prefix):
//
//	POST /v1/audio/speech          TTS: text -> audio
//	POST /v1/audio/transcriptions  ASR: audio -> text (same language, multipart)
//	POST /v1/audio/translations    ASR: audio -> English text (multipart)
//
// v0.1: the routes are registered, but there's no audio-capable Adapter yet;
// transcriptions / translations are multipart requests, same as image.
func registerAudioRoutes(engine *gin.Engine, deps Deps) {
	pre := llmRouteGroup(engine, deps)

	routes := []struct {
		path string
		mod  domain.Modality
	}{
		{"/v1/audio/speech", domain.ModalityTTS},
		{"/v1/audio/transcriptions", domain.ModalityASR},
		{"/v1/audio/translations", domain.ModalityASR},
	}
	for _, r := range routes {
		registerLLMRoute(pre, deps, routeSpec{Path: r.path, Protocol: domain.ProtoOpenAI, Modality: r.mod})
	}
}
