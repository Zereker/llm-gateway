// Package bedrock is the vendor implementation for AWS Bedrock. Two wire
// protocols are supported on the same vendor, selected by `ep.Protocol`:
//
//   - `protocol: anthropic` — InvokeModel / InvokeModelWithResponseStream,
//     Claude-on-Bedrock only, body is Anthropic Messages JSON as-is (reuses
//     openai_anthropic's translator + response handler). This is the
//     original, still-default path — see invokeSession below.
//   - `protocol: bedrock` — the Converse / ConverseStream API, a
//     model-agnostic wire shape AWS designed to work the same across
//     Claude/Titan/Nova/Llama/... (reuses internal/translator/openai_bedrock,
//     verified so far only against Claude traffic — see that package's doc
//     comment). Added because real-world recorded traffic
//     (langchain-ai/langchain-aws) for Bedrock overwhelmingly uses Converse,
//     not InvokeModel — see converseSession below.
//
// Both share:
//
//	Auth: AWS SigV4 (service=bedrock), credentials go through AWSSigV4Auth
//	      (access/secret/region).
//	URL:  the deployer fills routing.url with the full endpoint, including
//	      modelId + region host, e.g.
//	      https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20240620-v1:0/invoke
//	      https://bedrock-runtime.us-west-2.amazonaws.com/model/us.anthropic.claude-sonnet-5/converse
//
// **SigV4 uses the official aws-sdk-go-v2 signer**: edge cases in SigV4's
// canonical-URI encoding (Bedrock paths contain `:`) are extremely easy to get
// wrong by hand and impossible to verify offline against a real endpoint, so we
// rely on the official signer for correctness.
//
// **Streaming**: both paths use AWS event-stream binary framing for their
// streaming response, decoded by Factory.DecodeTransport (protocol.TransportDecoder,
// see stream.go / converse_stream.go) — which of the two frame shapes applies
// is sniffed from the actual request path Go's http.Client recorded on
// resp.Request (Factory.DecodeTransport has no other way to know which
// session produced this response, since it's dispatched by Factory type
// alone, not per-request state).
//
// Integration: the deployer configures the endpoint with `vendor: bedrock` +
// `protocol: anthropic` (InvokeModel) or `protocol: bedrock` (Converse) +
// `auth.type: aws-sigv4`. internal/builtin.NewLookup wires this package into
// the built-in lookup.
package bedrock

import (
	"context"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
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

// NewSession picks the InvokeModel or Converse session based on ep.Protocol.
func (Factory) NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (protocol.Session, error) {
	if ep.Protocol == domain.ProtoBedrock {
		return newConverseSession(c, ep, env), nil
	}

	return newSession(c, ep, env), nil
}
