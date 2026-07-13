package replay

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/internal/cassette"
	"github.com/zereker/llm-gateway/internal/protocol/bedrock"
	"github.com/zereker/llm-gateway/internal/translator/openai_bedrock"
)

// bedrockDirs are every vendored-corpus source that captured real Bedrock
// **Converse** API traffic (not InvokeModel — see
// internal/translator/openai_bedrock's doc comment for why the two need
// separate translators).
var bedrockDirs = []string{
	"bedrock/langchain-ai-langchain-aws",
}

// looksLikeConverseResponse reports whether body is a non-streaming Converse
// response (recognizable by a top-level "output" field, which a Converse
// *request* body never has). Streaming interactions are raw AWS event-stream
// binary framing, not JSON, so this always returns false for them by
// construction — TestReplayBedrockConverseStreaming covers those separately
// (it needs to run internal/protocol/bedrock's actual transport decoder
// first, which this vendorReplayConfig-based path has no hook for).
func looksLikeConverseResponse(body []byte) bool {
	return gjson.ValidBytes(body) && gjson.GetBytes(body, "output").Exists()
}

// TestReplayBedrockResponses feeds every real non-streaming Converse response
// body through openai_bedrock's response handler and asserts it doesn't
// error and produces well-formed OpenAI-shaped output.
func TestReplayBedrockResponses(t *testing.T) {
	runResponseReplay(t, vendorReplayConfig{
		dirs:       bedrockDirs,
		classify:   looksLikeConverseResponse,
		translator: openai_bedrock.New(),
		notApplicableReason: func(string) string {
			return `no interaction response body contained a top-level "output" field`
		},
	})
}

// TestGoldenBedrockToolCall pins the exact translated output of a real
// Converse tool-call turn against a hand-reviewed fixture -- a stricter
// companion to TestReplayBedrockResponses, which only checks the shape is
// valid and would not notice e.g. the tool name and arguments getting
// swapped, or the finish_reason mapping regressing.
func TestGoldenBedrockToolCall(t *testing.T) {
	its, err := cassette.LoadFS(vendored, "bedrock/langchain-ai-langchain-aws/test_agent_loop[v0].yaml.gz")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	h := openai_bedrock.New().NewResponseHandler()
	out, usage := feedResponse(t, h, its[0].ResponseBody, "golden/bedrock-tool-call")
	assertValidOpenAIChatOutput(t, out, "golden/bedrock-tool-call")
	if usage == nil || usage.Input != 446 || usage.Output != 55 || usage.Total != 501 {
		t.Fatalf("usage drifted from the real cassette's reported tokens: %+v", usage)
	}
	assertGolden(t, "bedrock-tool-call.json", out)
}

// TestReplayBedrockConverseStreaming covers the streaming half every mixed
// (streaming-turn + non-streaming-turn) cassette file contains, which
// TestReplayBedrockResponses's generic classify can't reach: a streaming
// interaction's ResponseBody is raw AWS event-stream binary framing, not
// JSON, so it has to go through internal/protocol/bedrock's actual
// TransportDecoder first (the same de-framing step the real request path
// runs) before openai_bedrock's response handler can parse it — mirrored
// here by hand since vendorReplayConfig's shared helper has no hook for an
// extra decode step. Identified by the interaction's own recorded URI
// (.../converse-stream) rather than sniffing the binary body.
func TestReplayBedrockConverseStreaming(t *testing.T) {
	f := bedrock.Factory{}
	tr := openai_bedrock.New()
	for _, dir := range bedrockDirs {
		files, err := loadVendoredDir(dir)
		if err != nil {
			t.Fatalf("LoadDir %s: %v", dir, err)
		}
		for _, rel := range cassette.SortedKeys(files) {
			path := dir + "/" + rel
			examined := false
			for i, it := range files[rel] {
				if !strings.HasSuffix(it.URI, "/converse-stream") || len(it.ResponseBody) == 0 {
					continue
				}
				examined = true
				t.Run(path+"#"+strconv.Itoa(i), func(t *testing.T) {
					req, err := http.NewRequest("POST", it.URI, nil)
					if err != nil {
						t.Fatalf("%s: build request: %v", path, err)
					}
					resp := &http.Response{
						Request: req,
						Header:  http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
						Body:    io.NopCloser(bytes.NewReader(it.ResponseBody)),
					}
					r := f.DecodeTransport(resp)
					if r == nil {
						t.Fatalf("%s: DecodeTransport returned nil for an eventstream response", path)
					}
					sse, err := io.ReadAll(r)
					if err != nil {
						t.Fatalf("%s: DecodeTransport read: %v", path, err)
					}
					h := tr.NewResponseHandler()
					out, usage := feedResponse(t, h, sse, path)
					assertValidOpenAIChatOutput(t, out, path)
					if usage == nil {
						t.Errorf("%s: expected non-nil usage", path)
					}
				})
			}
			if examined {
				claim(path)
			}
			// Not claiming on !examined is fine: TestReplayBedrockResponses
			// already claims/marks every file in this dir set via its own
			// non-streaming interaction (every file has at least one) --
			// double-claiming the same path from two suites is harmless.
		}
	}
}

// TestReplayBedrockRequestsAreWellFormed guards against a corrupted/truncated
// cassette silently masquerading as "out of scope" -- no reverse translator
// exists for Bedrock (upstream-only, like Gemini/Cohere).
func TestReplayBedrockRequestsAreWellFormed(t *testing.T) {
	runRequestWellFormedCheck(t, bedrockDirs)
}
