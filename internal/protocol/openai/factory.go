// Package openai is the vendor implementation of the OpenAI protocol (chat
// completions).
//
// Combined with the identity / anthropic_openai / responses_openai translators,
// it serves three src protocol combinations:
//
//	(openai, OpenAI)     ── identity passthrough
//	(openai, Anthropic)  ── anthropic_openai translator
//	(openai, Responses)  ── responses_openai translator
//
// internal/builtin.NewLookup wires the Factory into the built-in lookup under
// the vendor name "openai".
//
// This Factory is also reused directly for OpenAI-compatible upstreams
// (Azure / DeepSeek / vLLM-OpenAI / Ollama), as long as Endpoint.URL points
// at their respective /v1/chat/completions path — see aliases.go for the alias
// names NewLookup registers.
package openai

import (
	"context"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// VendorName is the key internal/builtin.NewLookup registers this Factory
// under — shared with Metadata().Vendor below so the two can't drift apart.
const VendorName = "openai"

// Factory implements protocol.Factory — used by protocol.Combine to wrap
// this into a Handler internally.
type Factory struct{}

// Metadata returns static metadata.
func (Factory) Metadata() protocol.Metadata {
	return protocol.Metadata{
		Vendor: VendorName,
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
