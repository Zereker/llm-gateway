package replay

import (
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/internal/translator/openai_cohere"
)

// cohereDirs are every vendored-corpus source that captured real
// api.cohere.com traffic. Like Gemini, Cohere is upstream-only in this
// gateway, so only the response direction (openai_cohere) has a translator
// to replay through.
var cohereDirs = []string{
	"cohere/langchain-ai-langchain-cohere",
}

// looksLikeCohereResponse reports whether body is a v2/chat response
// (streaming or not). Cohere's request shape is close to identical to
// OpenAI's own ({"model","messages",...}), so "finish_reason" (only ever
// present on a response) is the one reliable discriminator for the
// non-streaming case; the streaming case uses Cohere's own SSE event names.
func looksLikeCohereResponse(body []byte) bool {
	s := strings.TrimSpace(string(body))
	if strings.HasPrefix(s, "event:") || strings.HasPrefix(s, "data:") {
		return strings.Contains(s, `"type":"message-start"`)
	}
	return strings.Contains(s, `"finish_reason"`)
}

// TestReplayCohereResponses feeds every real Cohere v2/chat response body
// (non-streaming and SSE) through openai_cohere's response handler and
// asserts it doesn't error and produces well-formed OpenAI-shaped output.
func TestReplayCohereResponses(t *testing.T) {
	runResponseReplay(t, vendorReplayConfig{
		dirs:       cohereDirs,
		classify:   looksLikeCohereResponse,
		translator: openai_cohere.New(),
		notApplicableReason: func(rel string) string {
			if strings.Contains(rel, "test_documents") {
				// documents.yaml only exercises the "documents" RAG request
				// field, which OpenAI has no equivalent for and this pair
				// doesn't translate (see package doc comment) — there is no
				// chat response to replay here at all.
				return "RAG-only cassette (documents field); no chat/response content to translate"
			}
			return "no interaction response body classified as a Cohere v2/chat response"
		},
	})
}

// TestReplayCohereRequestsAreWellFormed mirrors
// TestReplayGeminiRequestsAreWellFormed: no reverse translator exists for
// Cohere (upstream-only), so this just guards against a corrupted cassette
// masquerading as "out of scope".
func TestReplayCohereRequestsAreWellFormed(t *testing.T) {
	runRequestWellFormedCheck(t, cohereDirs)
}
