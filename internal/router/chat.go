package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/internal/domain"
)

// registerChatRoutes registers the chat modality routes plus their dedicated
// middleware chain.
//
// Paths (each `.POST` carries its own full /v1 prefix, no outer group):
//
//	POST /v1/chat/completions   OpenAI / OpenAI-compat
//	POST /v1/messages           Anthropic
//	POST /v1/responses          OpenAI Responses (added in v1.0; the new protocol's input + instructions shape)
//
// **Protocol tagging**: each path mounts its own WithSourceProtocol before
// Envelope, pinning down "which protocol this path is." Envelope no longer
// does path-based heuristics (DefaultDetector has been removed).
//
// The shared security/quota/observability order lives in pipeline.go
// (llmRouteGroup + registerLLMRoute); each modality supplies only how it
// differs via routeSpec (path, protocol, modality, and its Cache stage). Chat
// routes use the exact-match response cache (deps.Cache).
func registerChatRoutes(engine *gin.Engine, deps Deps) {
	// Group the pre-Envelope shared middleware so it doesn't need to be
	// repeated on every POST; the "/" path prefix keeps the group from
	// introducing an extra URL segment.
	chat := llmRouteGroup(engine, deps)
	for _, spec := range []routeSpec{
		{Path: "/v1/chat/completions", Protocol: domain.ProtoOpenAI, Modality: domain.ModalityChat, Cache: deps.Cache},
		{Path: "/v1/messages", Protocol: domain.ProtoAnthropic, Modality: domain.ModalityChat, Cache: deps.Cache},
		{Path: "/v1/responses", Protocol: domain.ProtoResponses, Modality: domain.ModalityChat, Cache: deps.Cache},
	} {
		registerLLMRoute(chat, deps, spec)
	}
}
