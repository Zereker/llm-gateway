package router

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// registerImageRoutes registers the image modality routes plus their
// dedicated middleware chain.
//
// Paths (each `.POST` carries its own full /v1 prefix):
//
//	POST /v1/images/generations  OpenAI text-to-image
//	POST /v1/images/edits        OpenAI image edits (multipart/form-data)
//	POST /v1/images/variations   OpenAI image variations (multipart/form-data)
//
// v0.1: the routes + middleware are registered, but there's no image-capable
// Adapter yet; edits / variations are multipart requests, but the current
// DefaultParser only parses JSON — a multipart Parser will replace it once an
// image Adapter is wired in.
func registerImageRoutes(engine *gin.Engine, deps Deps) {
	pre := llmRouteGroup(engine, deps)
	for _, p := range []string{"/v1/images/generations", "/v1/images/edits", "/v1/images/variations"} {
		registerLLMRoute(pre, deps, routeSpec{Path: p, Protocol: domain.ProtoOpenAI, Modality: domain.ModalityImage})
	}
}
