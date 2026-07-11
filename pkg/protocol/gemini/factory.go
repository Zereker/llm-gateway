// Package gemini is the vendor Factory implementation for the Google Gemini protocol.
//
// init() registers itself into the protocol vendor registry under the vendor name "gemini".
//
// Two auth paths are supported (the adapter picks automatically based on ep.Auth.Type):
//   - AI Studio: auth.type = "gemini-key", a public API key (x-goog-api-key header)
//     URL: https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent
//   - Vertex AI: auth.type = "vertex-adc" (uses ADC) or "oauth2-sa" (embedded SA JSON)
//     URL: https://{location}-aiplatform.googleapis.com/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:generateContent
//
// **Client format**: clients send requests in OpenAI ChatCompletion format; the adapter
// translates internally to Gemini format for the upstream call, then translates the
// response back to OpenAI format. Clients are unaware of the vendor switch.
//
// **Supported**:
//   - Chat (system/user/assistant/text content)
//   - Streaming (client stream:true -> :streamGenerateContent?alt=sse, SSE chunks are
//     translated to OpenAI SSE, see openai_gemini responseHandler)
//
// **Not supported**:
//   - Function calling / tool_use
//   - Vision / multimodal (parts only support text)
//
// To wire it in, add a blank import in internal/builtin/builtin.go:
//
//	import _ "github.com/zereker/llm-gateway/pkg/protocol/gemini"
package gemini

import (
	"context"

	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// Factory implements protocol.Factory.
type Factory struct{}

// Metadata returns static metadata. endpoint.Protocol (deployer config) is usually
// ProtoGemini; when the client uses the OpenAI SDK, the dispatcher automatically
// wires in the openai_gemini translator.
func (Factory) Metadata() protocol.Metadata {
	return protocol.Metadata{
		Vendor:              "gemini",
		SupportedModalities: []domain.Modality{domain.ModalityChat},
	}
}

// NewSession builds a Session for this request.
//
// Whether to use the streaming endpoint is decided by reading the stream flag from the
// envelope's **original client body** — the translated Gemini body doesn't carry
// stream, so this reads the pre-translation RawBytes instead (OpenAI/Anthropic/Responses
// all use stream:true, and gjson's bool read is protocol-agnostic).
func (Factory) NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (protocol.Session, error) {
	tp, err := newTokenProvider(c, ep.Auth)
	if err != nil {
		return nil, err
	}
	streaming := env != nil && gjson.GetBytes(env.RawBytes, "stream").Bool()
	return newSession(c, ep, tp, streaming), nil
}

func init() {
	protocol.RegisterFactory("gemini", Factory{})
}
