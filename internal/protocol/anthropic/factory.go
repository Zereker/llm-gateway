// Package anthropic is the vendor Factory implementation for the Anthropic Messages protocol.
//
// internal/builtin.NewLookup wires it into the built-in lookup under the vendor name "anthropic".
//
// **Auth**: anthropic uses the `x-api-key` header (not Authorization Bearer). The schema
// uses AuthTypeXAPIKey. Various setups (company accounts, Bedrock relaying, etc.) all rely
// on x-api-key.
//
// **Required header**: `anthropic-version: 2023-06-01` (API version); the adapter adds it
// automatically.
//
// **Client-facing format**: clients send requests in OpenAI ChatCompletion format; the
// openai_anthropic translator converts to Anthropic Messages format for upstream, then
// translates the response back to OpenAI format.
//
// **Not supported as of v0.5**:
//   - Streaming (Anthropic SSE differs from OpenAI's; handled in a separate iteration)
//   - Function calling / tool_use
//   - Vision / multi-block content (only the text is taken from the content array)
//
// To onboard, add it to the factory map in internal/builtin.NewLookup under the
// vendor name "anthropic".
package anthropic

import (
	"context"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// Factory implements protocol.Factory.
type Factory struct{}

// Metadata returns static metadata. endpoint.Protocol (deployer config) determines which
// protocol the upstream speaks; it's typically set to ProtoAnthropic (identity passthrough)
// or ProtoOpenAI (client OpenAI → openai_anthropic translation).
func (Factory) Metadata() protocol.Metadata {
	return protocol.Metadata{
		Vendor:              "anthropic",
		SupportedModalities: []domain.Modality{domain.ModalityChat},
	}
}

// NewSession constructs a Session for this request.
func (Factory) NewSession(c context.Context, ep *domain.Endpoint, _ *domain.RequestEnvelope) (protocol.Session, error) {
	return newSession(c, ep), nil
}
