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

// Some OpenAI-compat vendors return `data` as an object rather than an array
// (a multimodal embeddings API) — the usage next to it must still be extracted.
// Regression: typing data as a slice failed the whole unmarshal and billed zero.
func TestOpenAI_ObjectShapedDataKeepsUsage(t *testing.T) {
	s := NewOpenAI()
	s.Feed([]byte(`{"created":1,"data":{"embedding":[0.1,0.2]},` +
		`"usage":{"prompt_tokens":527,"total_tokens":527,` +
		`"prompt_tokens_details":{"image_tokens":497,"text_tokens":30}}}`))

	u := s.Final()
	if u == nil {
		t.Fatal("expected usage despite object-shaped data, got nil")
	}

	if u.Input != 527 || u.Total != 527 {
		t.Errorf("usage = in=%d total=%d, want 527/527", u.Input, u.Total)
	}

	// Raw must be the upstream usage verbatim — vendor extension fields
	// (image/text token split) survive for downstream billing.
	var raw map[string]any
	_ = json.Unmarshal(u.Raw, &raw)

	details, _ := raw["prompt_tokens_details"].(map[string]any)
	if details["image_tokens"] != float64(497) || details["text_tokens"] != float64(30) {
		t.Errorf("vendor extension fields lost from Raw: %s", u.Raw)
	}
}

// The official Images API (gpt-image-1) and its compat vendors report the
// input_tokens/output_tokens family instead of prompt/completion — both
// official field families must normalize into Input/Output.
func TestOpenAI_ImagesFieldFamily(t *testing.T) {
	s := NewOpenAI()
	s.Feed([]byte(`{"created":1,"data":[{"url":"https://x/1.png"}],` +
		`"usage":{"generated_images":1,"output_tokens":16072,"total_tokens":16072}}`))

	u := s.Final()
	if u == nil {
		t.Fatal("expected usage from images body, got nil")
	}

	if u.Output != 16072 || u.Total != 16072 {
		t.Errorf("usage = out=%d total=%d, want 16072/16072", u.Output, u.Total)
	}

	var raw map[string]any
	_ = json.Unmarshal(u.Raw, &raw)

	if raw["generated_images"] != float64(1) {
		t.Errorf("vendor extension generated_images lost from Raw: %s", u.Raw)
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

// TestAnthropic_UnmodeledUsageFieldsSurviveInRaw regresses a bug where Final()
// hand-built Raw from only 4 known fields, silently dropping anything else in
// the upstream usage object. cache_creation's ephemeral_1h/5m_input_tokens
// breakdown and service_tier affect Anthropic's actual price (1h cache writes
// bill at a different multiplier than 5m) — downstream billing needs them in
// Raw verbatim, not just the 4 fields the gateway happens to model itself.
// TestAnthropic_ZeroUsageIsReportedNotDropped: a response that legitimately
// reports 0 input + 0 output tokens (e.g. a fully cache-read prompt with an
// empty completion) must yield a non-nil Usage of 0/0, not nil — nil is
// reserved for "no usage object was ever seen". Conflating the two hides a
// real (near-)zero from billing/observability.
func TestAnthropic_ZeroUsageIsReportedNotDropped(t *testing.T) {
	s := NewAnthropic()
	s.Feed([]byte(`{"type":"message","role":"assistant","content":[],` +
		`"usage":{"input_tokens":0,"output_tokens":0}}`))

	u := s.Final()
	if u == nil {
		t.Fatal("usage reported as 0/0 must not be dropped to nil")
	}
	if u.Input != 0 || u.Output != 0 || u.Total != 0 {
		t.Errorf("want 0/0/0, got input=%d output=%d total=%d", u.Input, u.Output, u.Total)
	}

	// A body with NO usage object still returns nil (the genuine "unseen" case).
	s2 := NewAnthropic()
	s2.Feed([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`))
	if u2 := s2.Final(); u2 != nil {
		t.Errorf("no usage object present must return nil, got %+v", u2)
	}
}

func TestAnthropic_UnmodeledUsageFieldsSurviveInRaw(t *testing.T) {
	s := NewAnthropic()
	s.Feed([]byte(`{"usage":{"input_tokens":100,"output_tokens":20,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":40,"ephemeral_1h_input_tokens":0},` +
		`"service_tier":"standard"}}`))

	u := s.Final()
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	var raw map[string]any
	if err := json.Unmarshal(u.Raw, &raw); err != nil {
		t.Fatalf("Raw not valid JSON: %v", err)
	}
	if _, ok := raw["cache_creation"]; !ok {
		t.Errorf("cache_creation breakdown dropped from Raw: %s", u.Raw)
	}
	if raw["service_tier"] != "standard" {
		t.Errorf("service_tier dropped from Raw: %s", u.Raw)
	}
}

// TestAnthropic_StreamingUnmodeledFieldSurvivesMerge is the streaming
// counterpart: message_start's usage carries the unmodeled field, and
// message_delta's output_tokens patch must merge into rawUsage rather than
// replacing it wholesale (which would drop message_start's fields).
func TestAnthropic_StreamingUnmodeledFieldSurvivesMerge(t *testing.T) {
	s := NewAnthropic()
	s.Feed([]byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":1,"service_tier":"standard"}}}` + "\n\n"))
	s.Feed([]byte("event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":12}}` + "\n\n"))

	u := s.Final()
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	var raw map[string]any
	if err := json.Unmarshal(u.Raw, &raw); err != nil {
		t.Fatalf("Raw not valid JSON: %v", err)
	}
	if raw["service_tier"] != "standard" {
		t.Errorf("service_tier from message_start dropped after message_delta merge: %s", u.Raw)
	}
	if raw["output_tokens"] != float64(12) {
		t.Errorf("output_tokens not patched to message_delta's value: %s", u.Raw)
	}
}

// TestGemini_UnmodeledUsageFieldsSurviveInRaw regresses a bug where Final()
// re-marshaled a narrow 3-field struct instead of the verbatim usageMetadata
// object, dropping thoughtsTokenCount (thinking-model reasoning tokens) and
// cachedContentTokenCount (context-cache discount) — both affect price.
func TestGemini_UnmodeledUsageFieldsSurviveInRaw(t *testing.T) {
	s := NewGemini()
	s.Feed([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":19,` +
		`"thoughtsTokenCount":4,"cachedContentTokenCount":3}}`))

	u := s.Final()
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	if u.Input != 10 || u.Output != 5 || u.Total != 19 {
		t.Errorf("core usage = %+v, want in=10 out=5 total=19", u)
	}
	var raw map[string]any
	if err := json.Unmarshal(u.Raw, &raw); err != nil {
		t.Fatalf("Raw not valid JSON: %v", err)
	}
	if raw["thoughtsTokenCount"] != float64(4) {
		t.Errorf("thoughtsTokenCount dropped from Raw: %s", u.Raw)
	}
	if raw["cachedContentTokenCount"] != float64(3) {
		t.Errorf("cachedContentTokenCount dropped from Raw: %s", u.Raw)
	}
}

// Anthropic non-streaming: cache tokens must be preserved in Raw (billed
// separately by Anthropic). Total includes cache_creation (Anthropic's own
// ITPM counts cache writes) but excludes cache_read (ITPM doesn't count cache
// hits, for all current models except Haiku 3.5).
func TestAnthropic_CacheTokensPreservedInRaw(t *testing.T) {
	s := NewAnthropic()
	s.Feed([]byte(`{"usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":40,"cache_read_input_tokens":8}}`))

	u := s.Final()
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	if u.Input != 100 || u.Output != 20 || u.Total != 160 {
		t.Errorf("core usage = %+v, want in=100 out=20 total=160 (100+40 cache_creation+20)", u)
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

// TestAnthropic_TotalIncludesCacheCreationExcludesCacheRead pins the TPM
// semantics: cache_creation_input_tokens counts toward Anthropic's own ITPM
// (a cache write is still tokens Anthropic had to process, billed at a
// premium), so it must be added into Total; cache_read_input_tokens does not
// count toward ITPM for any current model except Haiku 3.5, so it must stay
// excluded. Getting this backwards means our internal TPM bucket diverges
// from what actually throttles the account upstream.
func TestAnthropic_TotalIncludesCacheCreationExcludesCacheRead(t *testing.T) {
	s := NewAnthropic()
	s.Feed([]byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":1,"cache_creation_input_tokens":25,"cache_read_input_tokens":30}}}` + "\n\n"))
	s.Feed([]byte("event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":12}}` + "\n\n"))

	u := s.Final()
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	// want: input(50) + cache_creation(25) + output(12) = 87 — cache_read(30) excluded
	if u.Total != 87 {
		t.Errorf("Total = %d, want 87 (50 input + 25 cache_creation + 12 output, cache_read excluded)", u.Total)
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

// Some anthropic-compatible vendors report input_tokens 0 in message_start and
// ship the full usage in message_delta — the delta's non-zero input_tokens
// must win over the empty start value.
func TestAnthropic_FullUsageInMessageDelta(t *testing.T) {
	s := NewAnthropic()
	s.Feed([]byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":0,"output_tokens":1}}}` + "\n\n"))
	s.Feed([]byte("event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":21,"output_tokens":199}}` + "\n\n"))

	u := s.Final()
	if u == nil {
		t.Fatal("expected usage, got nil")
	}

	if u.Input != 21 || u.Output != 199 || u.Total != 220 {
		t.Errorf("usage = in=%d out=%d total=%d, want 21/199/220", u.Input, u.Output, u.Total)
	}
}
