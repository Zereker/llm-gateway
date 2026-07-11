// Package azureopenai is the vendor implementation for Azure OpenAI.
//
// Azure OpenAI's **wire protocol is just OpenAI's** (same request/response
// shape), so the endpoint's `protocol` field is still set to `openai` —
// we reuse OpenAI's translator + response handler + error classification
// (embeds openai.Factory to inherit Classify). The only differences are at
// the HTTP layer:
//
//	Auth header: `api-key: <key>` (not Authorization: Bearer; that's for
//	             Azure AD tokens).
//	URL:         carries an `?api-version=<ver>` query (the deployer either
//	             fills in the full Azure endpoint in routing.url, or fills
//	             base + routing.api_version and lets this adapter append
//	             api-version).
//
// Onboarding: when the deployer writes the endpoint, set `vendor: azure-openai`
// + `protocol: openai` + `auth.type: bearer` (payload.api_key = Azure key).
// internal/builtin.NewLookup wires this package into the built-in lookup.
package azureopenai

import (
	"context"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
	"github.com/zereker/llm-gateway/internal/protocol/openai"
)

// Factory embeds openai.Factory to inherit Classify (Azure returns
// OpenAI-shaped error JSON), and only overrides Metadata (vendor name) +
// NewSession (Azure HTTP layer).
type Factory struct {
	openai.Factory
}

// Metadata overrides the vendor name.
func (Factory) Metadata() protocol.Metadata {
	return protocol.Metadata{
		Vendor: "azure-openai",
		SupportedModalities: []domain.Modality{
			domain.ModalityChat,
			domain.ModalityEmbedding,
		},
	}
}

// NewSession uses the Azure session (api-key header + api-version URL).
func (Factory) NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (protocol.Session, error) {
	return newSession(c, ep, env), nil
}
