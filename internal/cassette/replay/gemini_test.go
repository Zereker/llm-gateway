package replay

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/internal/cassette"
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
	tr := openai_gemini.New()
	for _, dir := range geminiDirs {
		files, err := cassette.LoadDir(vendorRoot + "/" + dir)
		if err != nil {
			t.Fatalf("LoadDir %s: %v", dir, err)
		}
		for _, rel := range cassette.SortedKeys(files) {
			path := dir + "/" + rel
			interactions := files[rel]
			examined := false
			for i, it := range interactions {
				if len(it.ResponseBody) == 0 || !looksLikeGeminiResponse(it.ResponseBody) {
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
			if examined {
				claim(path)
			} else {
				markNotApplicable(path, "no interaction response body contained a \"candidates\" field")
			}
		}
	}
}

// TestReplayGeminiRequestsAreWellFormed doesn't translate Gemini request
// bodies (there's no such translator — see geminiDirs's doc comment) but
// still claims them: it just asserts they parse as valid JSON, so a
// corrupted/truncated cassette can't silently masquerade as "out of scope"
// via markNotApplicable.
func TestReplayGeminiRequestsAreWellFormed(t *testing.T) {
	for _, dir := range geminiDirs {
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
