package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
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
// Each modality lists its own required middleware; there's no shared
// buildChain extracted, because modalities are expected to diverge going
// forward (chat adds a Moderator / image adds a multipart Parser / audio adds
// an ASR-only ParamSpec, etc.). As of v0.1 the chains happen to be identical
// across modalities, but they're kept independent in code.
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
