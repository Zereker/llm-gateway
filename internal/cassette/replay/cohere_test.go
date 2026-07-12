package replay

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/internal/cassette"
	"github.com/zereker/llm-gateway/internal/translator/openai_cohere"
)

// cohereDirs are every vendor-cassettes source that captured real
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
	tr := openai_cohere.New()
	for _, dir := range cohereDirs {
		files, err := cassette.LoadDir(vendorRoot + "/" + dir)
		if err != nil {
			t.Fatalf("LoadDir %s: %v", dir, err)
		}
		for _, rel := range cassette.SortedKeys(files) {
			path := dir + "/" + rel
			interactions := files[rel]
			examined := false
			for i, it := range interactions {
				if len(it.ResponseBody) == 0 || !looksLikeCohereResponse(it.ResponseBody) {
					continue
				}
				examined = true
				i, it := i, it
				t.Run(path+"#"+strconv.Itoa(i), func(t *testing.T) {
					h := tr.NewResponseHandler()
					out, usage := feedResponse(t, h, it.ResponseBody, path)
					assertValidOpenAIChatOutput(t, out, path)
					if usage == nil {
						t.Errorf("%s: expected non-nil usage", path)
					}
				})
			}
			switch {
			case examined:
				claim(path)
			case strings.Contains(rel, "test_documents"):
				// documents.yaml only exercises the "documents" RAG request
				// field, which OpenAI has no equivalent for and this pair
				// doesn't translate (see package doc comment) — there is no
				// chat response to replay here at all.
				markNotApplicable(path, "RAG-only cassette (documents field); no chat/response content to translate")
			default:
				markNotApplicable(path, "no interaction response body classified as a Cohere v2/chat response")
			}
		}
	}
}

// TestReplayCohereRequestsAreWellFormed mirrors
// TestReplayGeminiRequestsAreWellFormed: no reverse translator exists for
// Cohere (upstream-only), so this just guards against a corrupted cassette
// masquerading as "out of scope".
func TestReplayCohereRequestsAreWellFormed(t *testing.T) {
	for _, dir := range cohereDirs {
		files, err := cassette.LoadDir(vendorRoot + "/" + dir)
		if err != nil {
			t.Fatalf("LoadDir %s: %v", dir, err)
		}
		for _, rel := range cassette.SortedKeys(files) {
			path := dir + "/" + rel
			for i, it := range files[rel] {
				if len(it.RequestBody) == 0 {
					continue
				}
				if !json.Valid(it.RequestBody) {
					t.Errorf("%s#%d: request body is not valid JSON", path, i)
				}
			}
		}
	}
}
