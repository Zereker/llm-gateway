// Package bedrock is the vendor implementation for AWS Bedrock (Anthropic Claude
// on Bedrock, supporting non-streaming InvokeModel + streaming
// InvokeModelWithResponseStream).
//
// **Wire protocol = Anthropic Messages**: the endpoint's `protocol` is set to
// `anthropic`, reusing the Anthropic translator + response handler (Bedrock's
// Claude response body is already Anthropic Messages JSON). What's Bedrock-specific
// is confined to the HTTP layer:
//
//	Auth: AWS SigV4 (service=bedrock), credentials go through AWSSigV4Auth
//	      (access/secret/region).
//	URL:  the deployer fills routing.url with the full invoke endpoint, including
//	      modelId + region host, e.g.
//	      https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20240620-v1:0/invoke
//	Body: Anthropic Messages with the top-level model field stripped (modelId is
//	      already in the URL) and anthropic_version filled in.
//
// **SigV4 uses the official aws-sdk-go-v2 signer**: edge cases in SigV4's
// canonical-URI encoding (Bedrock paths contain `:`) are extremely easy to get
// wrong by hand and impossible to verify offline against a real endpoint, so we
// rely on the official signer for correctness.
//
// **Streaming**: when the client sends stream:true, the
// InvokeModelWithResponseStream endpoint is used and the response is an AWS
// event-stream binary framing — Factory.DecodeTransport (protocol.TransportDecoder,
// see stream.go) decodes it into Anthropic SSE, which the openai_anthropic handler
// then translates into OpenAI SSE. The transport layer (frame decoding) is kept
// separate from the protocol layer (shape translation), reusing the existing
// Anthropic streaming translation.
//
// Integration: the deployer configures the endpoint with `vendor: bedrock` +
// `protocol: anthropic` + `auth.type: aws-sigv4`. cmd/gateway blank-imports this
// package.
package bedrock

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// Factory implements protocol.Factory. No custom Classify — Bedrock errors are
// shaped like AWS errors, so falling back to DefaultClassifier's status-based
// classification is sufficient.
type Factory struct{}

// Metadata returns static metadata.
func (Factory) Metadata() protocol.Metadata {
	return protocol.Metadata{
		Vendor:              "bedrock",
		SupportedModalities: []domain.Modality{domain.ModalityChat},
	}
}

// NewSession constructs a Bedrock session for this request.
func (Factory) NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (protocol.Session, error) {
	return newSession(c, ep, env), nil
}

func init() {
	protocol.RegisterFactory("bedrock", Factory{})
}
