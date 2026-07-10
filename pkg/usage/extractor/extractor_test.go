package extractor

import (
	"encoding/json"
	"strings"
	"testing"
)

// A CRLF-framed OpenAI stream (\r\n\r\n between events) must still yield usage;
// scanning only for "\n\n" would buffer forever and miss the final usage frame.
func TestOpenAI_CRLFFramedStreamUsage(t *testing.T) {
	s := NewOpenAI()
	frames := []string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}`,
		`data: [DONE]`,
	}
	// join with CRLF blank lines
	s.Feed([]byte(strings.Join(frames, "\r\n\r\n") + "\r\n\r\n"))

	u := s.Final()
	if u == nil {
		t.Fatal("expected usage from a CRLF-framed stream, got nil")
	}
	if u.Input != 11 || u.Output != 5 || u.Total != 16 {
		t.Errorf("usage = %+v, want in=11 out=5 total=16", u)
	}
}

// The same content with LF framing must of course still work (regression guard).
func TestOpenAI_LFFramedStreamUsage(t *testing.T) {
	s := NewOpenAI()
	s.Feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n" +
		"data: [DONE]\n\n"))
	u := s.Final()
	if u == nil || u.Total != 10 {
		t.Fatalf("LF-framed usage = %+v, want total=10", u)
	}
}

// Anthropic non-streaming: cache tokens must be preserved in Raw (billed
// separately by Anthropic) even though Total stays input+output.
func TestAnthropic_CacheTokensPreservedInRaw(t *testing.T) {
	s := NewAnthropic()
	s.Feed([]byte(`{"usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":40,"cache_read_input_tokens":8}}`))

	u := s.Final()
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	if u.Input != 100 || u.Output != 20 || u.Total != 120 {
		t.Errorf("core usage = %+v, want in=100 out=20 total=120", u)
	}
	var raw map[string]any
	if err := json.Unmarshal(u.Raw, &raw); err != nil {
		t.Fatalf("Raw not valid JSON: %v", err)
	}
	if raw["cache_creation_input_tokens"] != float64(40) {
		t.Errorf("cache_creation_input_tokens missing from Raw: %v", raw)
	}
	if raw["cache_read_input_tokens"] != float64(8) {
		t.Errorf("cache_read_input_tokens missing from Raw: %v", raw)
	}
}

// Anthropic streaming: cache tokens come in message_start and must survive to Final.
func TestAnthropic_CacheTokensStreaming(t *testing.T) {
	s := NewAnthropic()
	s.Feed([]byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":1,"cache_read_input_tokens":30}}}` + "\n\n"))
	s.Feed([]byte("event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":12}}` + "\n\n"))

	u := s.Final()
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	var raw map[string]any
	_ = json.Unmarshal(u.Raw, &raw)
	if raw["cache_read_input_tokens"] != float64(30) {
		t.Errorf("streaming cache_read_input_tokens not preserved: %v", raw)
	}
	if u.Output != 12 {
		t.Errorf("output = %d, want 12", u.Output)
	}
}
