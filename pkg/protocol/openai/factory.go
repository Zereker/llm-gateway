// Package openai is the vendor implementation of the OpenAI protocol (chat
// completions).
//
// init() registers into the protocol registry: (vendor, srcProto) → Handler,
// covering three src protocol combinations:
//
//	(openai, OpenAI)     ── identity passthrough
//	(openai, Anthropic)  ── anthropic_openai translator
//	(openai, Responses)  ── responses_openai translator
//
// To wire up OpenAI, add a blank import in internal/builtin/builtin.go:
//
//	import _ "github.com/zereker/llm-gateway/pkg/protocol/openai"
//
// This Factory is also reused directly for OpenAI-compatible upstreams
// (Azure / DeepSeek / vLLM-OpenAI / Ollama), as long as Endpoint.URL points
// at their respective /v1/chat/completions path — see aliases.go for alias
// registration.
package openai

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// Factory implements protocol.Factory — used by protocol.Combine to wrap
// this into a Handler internally.
type Factory struct{}

// Metadata returns static metadata.
func (Factory) Metadata() protocol.Metadata {
	return protocol.Metadata{
		Vendor: "openai",
		SupportedModalities: []domain.Modality{
			domain.ModalityChat,
			domain.ModalityEmbedding,
			domain.ModalityImage, // e.g. /v1/images/generations; the deployer points routing.url at the image API when configuring the endpoint
		},
	}
}

// NewSession builds a Session for this request.
func (Factory) NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (protocol.Session, error) {
	return newSession(c, ep, env), nil
}

func init() {
	protocol.RegisterFactory("openai", Factory{})
}
