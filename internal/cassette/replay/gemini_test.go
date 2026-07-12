package replay

import (
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/internal/translator/openai_gemini"
)

// geminiDirs are every vendor-cassettes source that captured real
// generativelanguage.googleapis.com traffic. Gemini is upstream-only in this
// gateway (no client-facing Gemini protocol — see CLAUDE.md's "Client
// Protocol Scope"), so unlike Anthropic there is no reverse-direction
// translator to replay request bodies through; only the response direction
// (openai_gemini) applies.
var geminiDirs = []string{
	"gemini/simonw-llm-gemini",
}

// looksLikeGeminiResponse reports whether body is a generateContent /
// streamGenerateContent response — recognizable by a top-level (or, for the
// raw non-SSE streaming array/alt=sse form, per-chunk) "candidates" field,
// which a Gemini *request* body never has (requests use "contents").
func looksLikeGeminiResponse(body []byte) bool {
	return strings.Contains(string(body), `"candidates"`)
}

// TestReplayGeminiResponses feeds every real Gemini response body — covering
// both wire shapes this cassette set contains (a single JSON object for
// generateContent, and the raw JSON-array-of-objects form generateContent's
// streaming counterpart uses without alt=sse) — through openai_gemini's
// response handler and asserts it doesn't error and produces well-formed
// OpenAI-shaped output.
func TestReplayGeminiResponses(t *testing.T) {
	runResponseReplay(t, vendorReplayConfig{
		dirs:       geminiDirs,
		classify:   looksLikeGeminiResponse,
		translator: openai_gemini.New(),
		notApplicableReason: func(string) string {
			return `no interaction response body contained a "candidates" field`
		},
	})
}

// TestReplayGeminiRequestsAreWellFormed doesn't translate Gemini request
// bodies (there's no such translator — see geminiDirs's doc comment) but
// still guards against a corrupted/truncated cassette silently masquerading
// as "out of scope".
func TestReplayGeminiRequestsAreWellFormed(t *testing.T) {
	runRequestWellFormedCheck(t, geminiDirs)
}
