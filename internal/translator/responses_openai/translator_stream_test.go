package responses_openai

import (
	"encoding/json"
	"strings"
	"testing"
)

// A streaming request must inject stream_options.include_usage so the upstream
// emits a final usage chunk (otherwise the request bills zero).
func TestTranslateRequest_StreamInjectsIncludeUsage(t *testing.T) {
	out, err := translateRequest([]byte(`{"model":"gpt-x","input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("bad output: %v", err)
	}
	so, ok := req["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Errorf("stream_options.include_usage not set: %v", req["stream_options"])
	}
}

// Non-streaming requests must NOT carry stream_options.
func TestTranslateRequest_NonStreamNoStreamOptions(t *testing.T) {
	out, _ := translateRequest([]byte(`{"model":"gpt-x","input":"hi"}`))
	if strings.Contains(string(out), "stream_options") {
		t.Errorf("non-stream request should not set stream_options: %s", out)
	}
}

// A streaming (SSE) upstream body must be aggregated into a valid Responses
// object with the concatenated text and the usage from the final chunk —
// not passed through as raw OpenAI chunks.
func TestFlush_AggregatesSSEStreamIntoResponses(t *testing.T) {
	h := (responsesOpenAI{}).NewResponseHandler()
	sse := strings.Join([]string{
		`data: {"id":"chatcmpl-abc","model":"gpt-x","choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"id":"chatcmpl-abc","model":"gpt-x","choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"id":"chatcmpl-abc","model":"gpt-x","choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"id":"chatcmpl-abc","choices":[{"delta":{}}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	if _, err := h.Feed([]byte(sse)); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	body, usage, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var resp responsesOutput
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("Flush output is not a valid Responses object: %v (body=%s)", err, body)
	}
	if resp.Object != "response" {
		t.Errorf("object = %q, want response", resp.Object)
	}
	if len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 ||
		resp.Output[0].Content[0].Text != "Hello" {
		t.Errorf("aggregated text wrong: %+v", resp.Output)
	}
	if resp.Usage.InputTokens != 4 || resp.Usage.OutputTokens != 2 {
		t.Errorf("usage not carried from final chunk: %+v", resp.Usage)
	}
	if usage == nil || usage.Total != 6 {
		t.Errorf("side-channel usage wrong: %+v", usage)
	}
}
