package openai_cohere

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/internal/domain"

	// makes the anthropic->cohere pivot reachable (composition requires anthropic->openai to already be registered).
	_ "github.com/zereker/llm-gateway/internal/translator/anthropic_openai"
)

func TestTranslateRequest(t *testing.T) {
	in := `{"model":"command-r","max_tokens":100,"temperature":0.3,"top_p":0.9,
	        "messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`
	out, err := translateRequest([]byte(in))
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	r := gjson.ParseBytes(out)
	if r.Get("model").String() != "command-r" {
		t.Errorf("model = %q", r.Get("model").String())
	}
	if r.Get("max_tokens").Int() != 100 || r.Get("temperature").Float() != 0.3 || r.Get("p").Float() != 0.9 {
		t.Errorf("params mapping wrong: %s", out)
	}
	if r.Get("stream").Bool() != false {
		t.Error("stream should be false")
	}
	if r.Get("messages.0.role").String() != "system" || r.Get("messages.1.content").String() != "hi" {
		t.Errorf("messages mapping wrong: %s", out)
	}
}

func TestTranslateRequest_MultimodalContentToText(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"part1 "},{"type":"text","text":"part2"}]}]}`
	out, _ := translateRequest([]byte(in))
	if c := gjson.GetBytes(out, "messages.0.content").String(); c != "part1 part2" {
		t.Errorf("content array should be flattened to text, got %q", c)
	}
}

func TestTranslateResponse(t *testing.T) {
	cohere := `{"id":"c-123","finish_reason":"COMPLETE",
	           "message":{"role":"assistant","content":[{"type":"text","text":"Hello "},{"type":"text","text":"world"}]},
	           "usage":{"billed_units":{"input_tokens":9,"output_tokens":5},"tokens":{"input_tokens":10,"output_tokens":5}}}`
	body, usage := translateResponse([]byte(cohere))
	r := gjson.ParseBytes(body)
	if r.Get("object").String() != "chat.completion" {
		t.Errorf("object = %q", r.Get("object").String())
	}
	if r.Get("choices.0.message.content").String() != "Hello world" {
		t.Errorf("content = %q", r.Get("choices.0.message.content").String())
	}
	if r.Get("choices.0.finish_reason").String() != "stop" {
		t.Errorf("finish_reason = %q, want stop", r.Get("choices.0.finish_reason").String())
	}
	if r.Get("usage.prompt_tokens").Int() != 10 || r.Get("usage.completion_tokens").Int() != 5 || r.Get("usage.total_tokens").Int() != 15 {
		t.Errorf("usage mapping wrong: %s", body)
	}
	// Cohere reports usage natively (we don't derive it), so Source must be
	// upstream, not extracted; Raw must preserve billed_units verbatim — it's
	// Cohere's actually-charged count, which downstream billing needs and can
	// differ from the raw tokens count.
	if usage == nil || usage.Total != 15 || usage.Source != domain.UsageSourceUpstream {
		t.Errorf("usage struct = %+v", usage)
	}
	if !gjson.GetBytes(usage.Raw, "billed_units").Exists() {
		t.Errorf("billed_units dropped from Raw: %s", usage.Raw)
	}
}

// Exercises the handler's Flush end-to-end after an upstream EOF.
func TestResponseHandler_BufferThenTranslate(t *testing.T) {
	h := &responseHandler{}
	// feed it in two chunks
	if b, _ := h.Feed([]byte(`{"id":"x","message":{"role":"assistant","content":[{"type":"text",`)); b != nil {
		t.Error("buffer-mode Feed should not return bytes")
	}
	h.Feed([]byte(`"text":"ok"}]},"finish_reason":"COMPLETE","usage":{"tokens":{"input_tokens":1,"output_tokens":2}}}`))
	body, usage, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if gjson.GetBytes(body, "choices.0.message.content").String() != "ok" || usage.Total != 3 {
		t.Errorf("flush result wrong: body=%s usage=%+v", body, usage)
	}
}

// An error response (message is a string) is passed through as-is, not translated.
func TestResponseHandler_ErrorPassthrough(t *testing.T) {
	h := &responseHandler{}
	errBody := `{"id":"x","message":"invalid api token"}`
	h.Feed([]byte(errBody))
	body, usage, _ := h.Flush()
	if string(body) != errBody {
		t.Errorf("error body should be passed through as-is, got %s", body)
	}
	if usage != nil {
		t.Error("error response should not have usage")
	}
}

// TestFinishReasonMap covers every documented Cohere v2 finish_reason value
// (https://docs.cohere.com/reference/chat) so a value like TOOL_CALL can't
// silently fall through a lowercase default into an invalid OpenAI enum
// member ("tool_call" instead of "tool_calls" — the bug this regresses).
func TestFinishReasonMap(t *testing.T) {
	for in, want := range map[string]string{
		"COMPLETE":      "stop",
		"MAX_TOKENS":    "length",
		"STOP_SEQUENCE": "stop",
		"TOOL_CALL":     "tool_calls",
		"ERROR":         "stop",
		"TIMEOUT":       "stop",
		"":              "stop",
	} {
		if got := mapFinishReason(in); got != want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCohereTranslatorMetadata(t *testing.T) {
	tr := New()
	if tr.Source() != domain.ProtoOpenAI || tr.Target() != domain.ProtoCohere {
		t.Fatalf("unexpected pair %s -> %s", tr.Source(), tr.Target())
	}
}

// Ensures translateRequest produces valid JSON.
func TestTranslateRequest_ValidJSON(t *testing.T) {
	out, _ := translateRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if !json.Valid(out) {
		t.Errorf("invalid JSON: %s", out)
	}
}
