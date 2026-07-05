// Package gemini is the vendor Factory implementation for the Google Gemini
// protocol.
//
// init() registers it with the protocol vendor registry under the vendor name
// "gemini".
//
// Two auth paths are supported (the adapter picks one automatically based on
// ep.Auth.Type):
//   - AI Studio: auth.type = "gemini-key", a public API key (x-goog-api-key header)
//     URL: https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent
//   - Vertex AI: auth.type = "vertex-adc" (uses ADC) or "oauth2-sa" (embedded SA JSON)
//     URL: https://{location}-aiplatform.googleapis.com/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:generateContent
//
// **Client format**: clients send requests in OpenAI ChatCompletion format;
// the adapter translates internally to Gemini format for the upstream call,
// then translates the response back to OpenAI format. Clients are unaware of
// the vendor switch.
//
// **Not supported in v0.5**:
//   - Streaming (Gemini uses :streamGenerateContent plus a different chunk
//     format, to be handled in a separate iteration)
//   - Function calling / tool_use
//   - Vision / multimodal (parts only support text)
//
// To wire it in, add a blank import in cmd/gateway/main.go:
//
//	import _ "github.com/zereker/llm-gateway/pkg/protocol/gemini"
package gemini

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// Factory implements protocol.Factory.
type Factory struct{}

// Metadata returns static metadata. endpoint.Protocol (deployer config) is
// usually ProtoGemini; when the client uses the OpenAI SDK, the dispatcher
// automatically wires in the openai_gemini translation.
func (Factory) Metadata() protocol.Metadata {
	return protocol.Metadata{
		Vendor:              "gemini",
		SupportedModalities: []domain.Modality{domain.ModalityChat},
	}
}

// NewSession constructs a Session for this request.
//
// The envelope isn't needed in the slim adapter model — the translator has
// already consumed and translated the raw body. The parameter is kept for
// protocol.Factory interface compatibility; session doesn't store it.
func (Factory) NewSession(c context.Context, ep *domain.Endpoint, _ *domain.RequestEnvelope) (protocol.Session, error) {
	tp, err := newTokenProvider(c, ep.Auth)
	if err != nil {
		return nil, err
	}
	return newSession(c, ep, tp), nil
}

func init() {
	protocol.RegisterFactory("gemini", Factory{})
}
