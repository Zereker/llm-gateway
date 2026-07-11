package openai_gemini

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestTranslateRequest_MultipleSystemMessagesMerge regresses a bug where each
// system message replaced systemInstruction wholesale, so a client (or an
// injected middleware reminder) sending more than one system message silently
// lost all but the last.
func TestTranslateRequest_MultipleSystemMessagesMerge(t *testing.T) {
	body := []byte(`{"model":"gemini-x","messages":[
		{"role":"system","content":"be terse"},
		{"role":"system","content":"never mention prices"},
		{"role":"user","content":"hi"}
	]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translateRequest error: %v", err)
	}
	parts := gjson.GetBytes(out, "systemInstruction.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("systemInstruction.parts = %d entries, want 2: %s", len(parts), out)
	}
	if parts[0].Get("text").String() != "be terse" || parts[1].Get("text").String() != "never mention prices" {
		t.Errorf("system parts wrong: %s", out)
	}
}

// TestMapFinishReason_Completeness covers every documented Gemini
// Candidate.FinishReason value so a value the mapping doesn't know about
// can't silently collapse into "stop" and hide a safety block or a malformed
// tool call behind a reply that looks like a clean completion.
func TestMapFinishReason_Completeness(t *testing.T) {
	openAIValidFinishReasons := map[string]bool{
		"stop": true, "length": true, "tool_calls": true, "content_filter": true,
	}
	for in, want := range map[string]string{
		"STOP":                      "stop",
		"MAX_TOKENS":                "length",
		"SAFETY":                    "content_filter",
		"RECITATION":                "content_filter",
		"LANGUAGE":                  "content_filter",
		"BLOCKLIST":                 "content_filter",
		"PROHIBITED_CONTENT":        "content_filter",
		"SPII":                      "content_filter",
		"MALFORMED_FUNCTION_CALL":   "tool_calls",
		"OTHER":                     "stop",
		"FINISH_REASON_UNSPECIFIED": "stop",
		"":                          "stop",
	} {
		got := mapFinishReason(in)
		if got != want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", in, got, want)
		}
		if !openAIValidFinishReasons[got] {
			t.Errorf("mapFinishReason(%q) = %q, not a valid OpenAI finish_reason", in, got)
		}
	}
}

// A success response whose body happens to contain "error" should not be misdetected as
// an error (the old byte-scanning bug); only a genuine error body should be.
func TestIsGeminiError(t *testing.T) {
	success := []byte(`{"candidates":[{"content":{"parts":[{"text":"error"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":1,"totalTokenCount":9}}`)
	if isGeminiError(success) {
		t.Error("a success response whose body contains \"error\" was misdetected as an error")
	}
	errBody := []byte(`{"error":{"code":400,"message":"bad","status":"INVALID_ARGUMENT"}}`)
	if !isGeminiError(errBody) {
		t.Error("a genuine error response was not recognized")
	}
}

// When a success response body is "error", it should translate normally and carry usage
// (not take the error passthrough path).
func TestResponseHandler_JSON_ContentIsError(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	body := `{"candidates":[{"content":{"parts":[{"text":"error"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":1,"totalTokenCount":9}}`
	_, _ = h.Feed([]byte(body))
	out, usage, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !strings.Contains(string(out), `"object":"chat.completion"`) || !strings.Contains(string(out), `"content":"error"`) {
		t.Errorf("should translate to OpenAI shape, got: %s", out)
	}
	if usage == nil || usage.Total != 9 {
		t.Errorf("usage should be preserved, got %+v", usage)
	}
}

// Safety block (no candidates + promptFeedback.blockReason): non-streaming choices
// non-null + content_filter.
func TestResponseHandler_JSON_SafetyBlock(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	body := `{"promptFeedback":{"blockReason":"SAFETY"}}`
	_, _ = h.Feed([]byte(body))
	out, _, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	choices := gjson.GetBytes(out, "choices")
	if !choices.IsArray() || len(choices.Array()) != 1 {
		t.Fatalf("choices should be an array with one element (not null), got: %s", out)
	}
	if choices.Array()[0].Get("finish_reason").String() != "content_filter" {
		t.Errorf("finish_reason should be content_filter, got: %s", out)
	}
}

// Streaming safety block: not an empty stream, sends a content_filter closing chunk.
func TestResponseHandler_SSE_SafetyBlock(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	chunk := "data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\n"
	out, _ := h.Feed([]byte(chunk))
	final, _, _ := h.Flush()
	all := string(out) + string(final)
	if !strings.Contains(all, `"finish_reason":"content_filter"`) {
		t.Errorf("a streaming block should emit a content_filter chunk, got: %s", all)
	}
	if !strings.Contains(all, "data: [DONE]") {
		t.Errorf("should have [DONE], got: %s", all)
	}
}

// Streaming: Gemini SSE chunk -> OpenAI chat.completion.chunk SSE + usage extracted from
// the last frame.
func TestResponseHandler_SSE(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()

	// Two Gemini SSE chunks: first frame content, last frame content + finishReason +
	// usageMetadata.
	chunk1 := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}],\"role\":\"model\"},\"index\":0}]}\n\n"
	chunk2 := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" world\"}],\"role\":\"model\"},\"finishReason\":\"STOP\",\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":2,\"totalTokenCount\":7}}\n\n"

	out1, err := h.Feed([]byte(chunk1))
	if err != nil {
		t.Fatalf("Feed1: %v", err)
	}
	out2, err := h.Feed([]byte(chunk2))
	if err != nil {
		t.Fatalf("Feed2: %v", err)
	}
	final, usage, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	all := string(out1) + string(out2) + string(final)

	for _, want := range []string{
		`"object":"chat.completion.chunk"`,
		`"role":"assistant"`,
		`"content":"Hello"`,
		`"content":" world"`,
		`"finish_reason":"stop"`,
		"data: [DONE]",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("SSE output missing %q\nfull text:\n%s", want, all)
		}
	}
	// role delta is only sent once.
	if n := strings.Count(all, `"role":"assistant"`); n != 1 {
		t.Errorf("role delta should be sent only once, got %d", n)
	}
	if usage == nil || usage.Input != 5 || usage.Output != 2 || usage.Total != 7 {
		t.Errorf("usage=%+v, want in=5 out=2 total=7", usage)
	}
}

// Across a Feed boundary: the half-line stays in lineBuf and is reassembled on the next
// Feed.
func TestResponseHandler_SSE_SplitAcrossFeeds(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	full := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hi\"}]},\"index\":0}]}\n\n"
	// split in the middle
	mid := len(full) / 2
	_, _ = h.Feed([]byte(full[:mid]))
	out, _ := h.Feed([]byte(full[mid:]))
	final, _, _ := h.Flush()
	all := string(out) + string(final)
	if !strings.Contains(all, `"content":"Hi"`) {
		t.Errorf("content was lost across the Feed boundary, got: %s", all)
	}
}

// The non-streaming JSON path still works normally (buffer-then-translate).
func TestResponseHandler_JSON(t *testing.T) {
	h := openaiGemini{}.NewResponseHandler()
	body := `{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`
	if _, err := h.Feed([]byte(body)); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	out, usage, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"object":"chat.completion"`) || !strings.Contains(s, `"content":"answer"`) {
		t.Errorf("JSON translation output is unexpected: %s", s)
	}
	if strings.Contains(s, "chunk") {
		t.Errorf("non-streaming output should not be chunk shape: %s", s)
	}
	if usage == nil || usage.Total != 4 {
		t.Errorf("usage=%+v, want total=4", usage)
	}
}
