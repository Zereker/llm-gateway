package replay

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/internal/cassette"
	"github.com/zereker/llm-gateway/internal/translator/anthropic_openai"
	"github.com/zereker/llm-gateway/internal/translator/openai_anthropic"
)

// anthropicDirs are every vendored-corpus source that captured real
// api.anthropic.com traffic.
var anthropicDirs = []string{
	"anthropic/simonw-llm-anthropic",
	"anthropic/langchain-ai-langchain",
}

// looksLikeAnthropicMessageResponse reports whether body is a Messages API
// response (streaming or not), including an error response — this cassette
// set doesn't contain anything else, but the check keeps the classifier
// honest instead of assuming.
func looksLikeAnthropicMessageResponse(body []byte) bool {
	s := strings.TrimSpace(string(body))
	if strings.HasPrefix(s, "event:") {
		return strings.Contains(s, `"type":"message_start"`)
	}
	return strings.Contains(s, `"type":"message"`) || strings.Contains(s, `"type":"error"`)
}

func looksLikeAnthropicMessageRequest(body []byte) bool {
	var probe struct {
		Model    string `json:"model"`
		Messages []any  `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Model != "" && probe.Messages != nil
}

// TestReplayAnthropicResponses feeds every real Anthropic response body
// (non-streaming and SSE, across both vendored sources) through
// openai_anthropic's response handler — the Anthropic-response ->
// OpenAI-response direction — and asserts it doesn't error and produces
// well-formed output the OpenAI SDK could parse.
func TestReplayAnthropicResponses(t *testing.T) {
	runResponseReplay(t, vendorReplayConfig{
		dirs:       anthropicDirs,
		classify:   looksLikeAnthropicMessageResponse,
		translator: openai_anthropic.New(),
		notApplicableReason: func(string) string {
			return "no interaction body classified as an Anthropic Messages response (request-only or unrecognized shape)"
		},
		// An error response has no usage; everything else should have gotten
		// one from extractor.Anthropic.
		skipUsageCheck: func(body []byte) bool {
			return strings.Contains(string(body), `"type":"error"`)
		},
	})
}

// TestGoldenAnthropicThinking pins the exact translated output of a real
// extended-thinking stream (thinking text -> reasoning_content deltas, the
// signature -> a reasoning_signature delta, then the final answer -> content
// deltas) against a hand-reviewed fixture -- a stricter companion to
// TestReplayAnthropicResponses, which only checks the shape is valid and
// would not notice e.g. thinking and answer text getting swapped.
func TestGoldenAnthropicThinking(t *testing.T) {
	its, err := cassette.LoadFS(vendored, "anthropic/simonw-llm-anthropic/test_stream_events_thinking.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	h := openai_anthropic.New().NewResponseHandler()
	out, usage := feedResponse(t, h, its[0].ResponseBody, "golden/anthropic-thinking")
	assertValidOpenAIChatOutput(t, out, "golden/anthropic-thinking")
	if usage == nil || usage.Input != 46 || usage.Output != 133 || usage.Total != 179 {
		t.Fatalf("usage drifted from the real cassette's reported tokens: %+v", usage)
	}
	assertGolden(t, "anthropic-thinking.txt", out)
}

// TestReplayAnthropicRequests feeds every real Anthropic Messages *request*
// body through anthropic_openai's TranslateRequest — the Anthropic-request ->
// OpenAI-upstream direction — and asserts it translates without error into
// something json.Valid.
func TestReplayAnthropicRequests(t *testing.T) {
	tr := anthropic_openai.New()
	for _, dir := range anthropicDirs {
		files, err := loadVendoredDir(dir)
		if err != nil {
			t.Fatalf("LoadDir %s: %v", dir, err)
		}
		for _, rel := range cassette.SortedKeys(files) {
			path := dir + "/" + rel
			interactions := files[rel]
			examined := false
			for i, it := range interactions {
				if len(it.RequestBody) == 0 || !looksLikeAnthropicMessageRequest(it.RequestBody) {
					continue
				}
				examined = true
				t.Run(path+"#"+strconv.Itoa(i), func(t *testing.T) {
					out, err := tr.TranslateRequest(it.RequestBody)
					if err != nil {
						t.Fatalf("%s: TranslateRequest error: %v", path, err)
					}
					if !json.Valid(out) {
						t.Fatalf("%s: TranslateRequest produced invalid JSON: %s", path, out)
					}
				})
			}
			if examined {
				claim(path)
			}
			// Not claiming here on !examined is fine: TestReplayAnthropicResponses
			// (or another suite) already claims/marks every file in this dir set;
			// double-claiming the same path from two suites is harmless (claim is
			// idempotent), so we don't need an else-branch mirroring the one above.
		}
	}
}
