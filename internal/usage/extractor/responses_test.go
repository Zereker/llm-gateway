package extractor

import (
	"encoding/json"
	"testing"
)

// A non-streaming Responses body carries usage with input_tokens/output_tokens
// (NOT prompt_tokens/completion_tokens) — the regression here was billing zero
// in/out because the OpenAI chat extractor was applied to a Responses body.
func TestResponses_NonStreamBodyUsage(t *testing.T) {
	s := NewResponses()
	s.Feed([]byte(`{"id":"resp_1","object":"response","status":"completed",` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],` +
		`"usage":{"input_tokens":1689,"output_tokens":532,"total_tokens":2221,` +
		`"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":469}}}`))

	u := s.Final()
	if u == nil {
		t.Fatal("expected usage from a Responses body, got nil")
	}

	if u.Input != 1689 || u.Output != 532 || u.Total != 2221 {
		t.Errorf("usage = in=%d out=%d total=%d, want 1689/532/2221", u.Input, u.Output, u.Total)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(u.Raw, &raw); err != nil {
		t.Fatalf("Raw not valid JSON: %v", err)
	}

	if _, ok := raw["output_tokens_details"]; !ok {
		t.Errorf("Raw lost output_tokens_details: %s", u.Raw)
	}
}

// In a Responses SSE stream the usage arrives only on the final
// response.completed event, nested under its `response` field.
func TestResponses_StreamCompletedEventUsage(t *testing.T) {
	s := NewResponses()
	s.Feed([]byte("event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"1,2"}` + "\n\n"))
	s.Feed([]byte("event: response.output_text.done\n" +
		`data: {"type":"response.output_text.done","text":"1,2,3"}` + "\n\n"))
	s.Feed([]byte("event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed",` +
		`"usage":{"input_tokens":21,"output_tokens":9,"total_tokens":30}}}` + "\n\n"))

	u := s.Final()
	if u == nil {
		t.Fatal("expected usage from response.completed, got nil")
	}

	if u.Input != 21 || u.Output != 9 || u.Total != 30 {
		t.Errorf("usage = in=%d out=%d total=%d, want 21/9/30", u.Input, u.Output, u.Total)
	}
}

// Delta events without usage must not fabricate one.
func TestResponses_StreamWithoutUsage(t *testing.T) {
	s := NewResponses()
	s.Feed([]byte("event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n"))

	if u := s.Final(); u != nil {
		t.Errorf("expected nil usage, got %+v", u)
	}
}
