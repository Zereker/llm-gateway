package openai_cohere

import (
	"encoding/json"
	"strings"
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

// TestTranslateRequest_ToolsPassthrough: Cohere v2's tool shape is identical
// to OpenAI's, so tools should pass through field-for-field.
func TestTranslateRequest_ToolsPassthrough(t *testing.T) {
	in := `{"model":"m","tools":[{"type":"function","function":{"name":"get_weather","description":"gets weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
	        "messages":[{"role":"user","content":"weather in SF?"}]}`
	out, err := translateRequest([]byte(in))
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	r := gjson.ParseBytes(out)
	if r.Get("tools.0.function.name").String() != "get_weather" {
		t.Errorf("tools not passed through: %s", out)
	}
	if r.Get("tools.0.function.parameters.properties.city.type").String() != "string" {
		t.Errorf("tool parameters schema lost: %s", out)
	}
}

// TestTranslateRequest_ToolChoice covers the lossy OpenAI -> Cohere v2
// tool_choice mapping: Cohere only has REQUIRED/NONE, no "auto" and no way to
// force one specific named tool.
func TestTranslateRequest_ToolChoice(t *testing.T) {
	cases := []struct {
		name   string
		choice string
		want   string // "" means tool_choice omitted
	}{
		{"required", `"required"`, `"REQUIRED"`},
		{"none", `"none"`, `"NONE"`},
		{"auto_omitted", `"auto"`, ``},
		{"named_function_forces_required", `{"type":"function","function":{"name":"foo"}}`, `"REQUIRED"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"m","tool_choice":` + tc.choice + `,"messages":[{"role":"user","content":"hi"}]}`)
			out, err := translateRequest(body)
			if err != nil {
				t.Fatalf("translateRequest: %v", err)
			}
			got := gjson.GetBytes(out, "tool_choice")
			if tc.want == "" {
				if got.Exists() {
					t.Errorf("want tool_choice omitted, got %s", got.Raw)
				}
				return
			}
			if got.Raw != tc.want {
				t.Errorf("tool_choice = %s, want %s", got.Raw, tc.want)
			}
		})
	}
}

// TestTranslateRequest_AssistantToolCallsHistory: a prior assistant turn that
// called a tool must translate to Cohere's {tool_calls:[...], content omitted
// when there's no accompanying text} shape.
func TestTranslateRequest_AssistantToolCallsHistory(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":"weather in SF?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"}
	]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	r := gjson.ParseBytes(out)
	if r.Get("messages.1.content").Exists() {
		t.Errorf("assistant content should be omitted (tool_calls only), got %s", r.Get("messages.1.content").Raw)
	}
	if r.Get("messages.1.tool_calls.0.id").String() != "call_1" ||
		r.Get("messages.1.tool_calls.0.function.name").String() != "get_weather" ||
		r.Get("messages.1.tool_calls.0.function.arguments").String() != `{"city":"SF"}` {
		t.Errorf("assistant tool_calls mapping wrong: %s", out)
	}
	if r.Get("messages.2.role").String() != "tool" || r.Get("messages.2.tool_call_id").String() != "call_1" || r.Get("messages.2.content").String() != "sunny" {
		t.Errorf("tool result message mapping wrong (missing tool_call_id?): %s", out)
	}
}

// TestTranslateRequest_SamplingParamsForwarded regresses a bug where stop /
// frequency_penalty / presence_penalty / seed / n were parsed from the client
// body but never copied into the Cohere request — json.Unmarshal into
// openaiReq silently dropped them since cohereReq had no matching fields, so
// a client tuning repetition penalties or relying on stop got a silent no-op.
func TestTranslateRequest_SamplingParamsForwarded(t *testing.T) {
	in := `{"model":"m","stop":["\n\n","STOP"],"frequency_penalty":0.5,"presence_penalty":0.2,"seed":42,"n":3,
	        "messages":[{"role":"user","content":"hi"}]}`
	out, err := translateRequest([]byte(in))
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	r := gjson.ParseBytes(out)
	stops := r.Get("stop_sequences").Array()
	if len(stops) != 2 || stops[0].String() != "\n\n" || stops[1].String() != "STOP" {
		t.Errorf("stop_sequences = %s, want [\"\\n\\n\",\"STOP\"]", r.Get("stop_sequences").Raw)
	}
	if r.Get("frequency_penalty").Float() != 0.5 {
		t.Errorf("frequency_penalty = %v", r.Get("frequency_penalty"))
	}
	if r.Get("presence_penalty").Float() != 0.2 {
		t.Errorf("presence_penalty = %v", r.Get("presence_penalty"))
	}
	if r.Get("seed").Int() != 42 {
		t.Errorf("seed = %v", r.Get("seed"))
	}
	if r.Get("num_generations").Int() != 3 {
		t.Errorf("num_generations = %v, want 3 (from n)", r.Get("num_generations"))
	}
}

// A single string stop value must also translate to a one-element array.
func TestTranslateRequest_StopStringForwarded(t *testing.T) {
	in := `{"model":"m","stop":"STOP","messages":[{"role":"user","content":"hi"}]}`
	out, err := translateRequest([]byte(in))
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	stops := gjson.GetBytes(out, "stop_sequences").Array()
	if len(stops) != 1 || stops[0].String() != "STOP" {
		t.Errorf("stop_sequences = %v, want [\"STOP\"]", stops)
	}
}

func TestTranslateRequest_MultimodalContentToText(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"part1 "},{"type":"text","text":"part2"}]}]}`
	out, _ := translateRequest([]byte(in))
	if c := gjson.GetBytes(out, "messages.0.content").String(); c != "part1 part2" {
		t.Errorf("content array should be flattened to text, got %q", c)
	}
}

// TestTranslateRequest_ImageURLPassthrough: Cohere v2's image_url content
// part is structurally identical to OpenAI's, so it should pass through
// as an array (not collapse to text-only) once an image is present. The
// base64 payload is a real captured image (a tiny PNG) from
// simonw/llm-anthropic's test_image_prompt.yaml cassette (Apache 2.0).
func TestTranslateRequest_ImageURLPassthrough(t *testing.T) {
	const realPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAKYAAAEaAgMAAADmmcReAAAACVBMVEX///8A/wD+AQASdAFKAAAAR0lEQVR42u3YMREAMAjAwC5d6q8mUYkEVuA+8yvIkVr0oghFURRFURRFURRFUdRCkSRJM7u/CEVRFEVRFEVRFEXRpdQXkcaVBRUPn8UJn6QAAAAASUVORK5CYII="
	in := `{"model":"m","messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"data:image/png;base64,` + realPNGBase64 + `"}},
		{"type":"text","text":"Describe image in three words"}
	]}]}`
	out, err := translateRequest([]byte(in))
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	content := gjson.GetBytes(out, "messages.0.content")
	if !content.IsArray() {
		t.Fatalf("content should stay an array when an image is present, got: %s", content.Raw)
	}
	parts := content.Array()
	if len(parts) != 2 {
		t.Fatalf("want 2 parts, got %d: %s", len(parts), content.Raw)
	}
	if parts[0].Get("type").String() != "image_url" || parts[0].Get("image_url.url").String() != "data:image/png;base64,"+realPNGBase64 {
		t.Errorf("image_url part not passed through: %s", parts[0].Raw)
	}
	if parts[1].Get("type").String() != "text" || parts[1].Get("text").String() != "Describe image in three words" {
		t.Errorf("text part wrong: %s", parts[1].Raw)
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

// TestTranslateResponse_ToolCalls: message.tool_calls -> OpenAI tool_calls,
// content:null (no accompanying text), finish_reason:tool_calls.
func TestTranslateResponse_ToolCalls(t *testing.T) {
	cohere := `{"id":"c-123","finish_reason":"TOOL_CALL",
	           "message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},
	           "usage":{"tokens":{"input_tokens":10,"output_tokens":5}}}`
	body, _ := translateResponse([]byte(cohere))
	r := gjson.ParseBytes(body)
	if r.Get("choices.0.finish_reason").String() != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", r.Get("choices.0.finish_reason").String())
	}
	if !r.Get("choices.0.message.content").IsObject() && r.Get("choices.0.message.content").Type != gjson.Null {
		t.Errorf("content should be null, got %s", r.Get("choices.0.message.content").Raw)
	}
	tc := r.Get("choices.0.message.tool_calls.0")
	if tc.Get("id").String() != "call_1" || tc.Get("type").String() != "function" ||
		tc.Get("function.name").String() != "get_weather" || tc.Get("function.arguments").String() != `{"city":"SF"}` {
		t.Errorf("tool_calls mapping wrong: %s", body)
	}
}

// TestTranslateResponse_ToolCallsWithText: text alongside tool_calls must
// survive as content (not forced to null).
func TestTranslateResponse_ToolCallsWithText(t *testing.T) {
	cohere := `{"id":"c-123","finish_reason":"TOOL_CALL",
	           "message":{"role":"assistant","content":[{"type":"text","text":"let me check"}],
	                      "tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
	           "usage":{"tokens":{"input_tokens":10,"output_tokens":5}}}`
	body, _ := translateResponse([]byte(cohere))
	r := gjson.ParseBytes(body)
	if r.Get("choices.0.message.content").String() != "let me check" {
		t.Errorf("content = %q, want %q", r.Get("choices.0.message.content").String(), "let me check")
	}
	if r.Get("choices.0.message.tool_calls.0.id").String() != "call_1" {
		t.Errorf("tool_calls missing: %s", body)
	}
}

// TestCohereStreamTranslate_ToolCalls: tool-call-start -> tool-call-delta
// (arguments streamed incrementally) -> tool-call-end must assemble into
// OpenAI's streaming tool_calls delta chunk shape, indexed for parallel calls.
func TestCohereStreamTranslate_ToolCalls(t *testing.T) {
	h := &responseHandler{}
	sse := `data: {"type":"message-start"}

data: {"type":"tool-plan-delta","delta":{"message":{"tool_plan":"I should check the weather"}}}

data: {"type":"tool-call-start","index":0,"delta":{"message":{"tool_calls":{"id":"call_1","type":"function","function":{"name":"get_weather"}}}}}

data: {"type":"tool-call-delta","index":0,"delta":{"message":{"tool_calls":{"function":{"arguments":"{\"city\":"}}}}}

data: {"type":"tool-call-delta","index":0,"delta":{"message":{"tool_calls":{"function":{"arguments":"\"SF\"}"}}}}}

data: {"type":"tool-call-end","index":0}

data: {"type":"message-end","delta":{"finish_reason":"TOOL_CALL","usage":{"tokens":{"input_tokens":5,"output_tokens":8}}}}

`
	out, _ := h.Feed([]byte(sse))
	fin, usage, _ := h.Flush()
	all := string(out) + string(fin)

	var id, name string
	var argsBuf strings.Builder
	for _, line := range strings.Split(all, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		payload := line[len("data: "):]
		tc := gjson.Get(payload, "choices.0.delta.tool_calls.0")
		if !tc.Exists() {
			continue
		}
		if v := tc.Get("id").String(); v != "" {
			id = v
		}
		if v := tc.Get("function.name").String(); v != "" {
			name = v
		}
		argsBuf.WriteString(tc.Get("function.arguments").String())
	}
	if id != "call_1" || name != "get_weather" {
		t.Errorf("id=%q name=%q, want call_1/get_weather", id, name)
	}
	if argsBuf.String() != `{"city":"SF"}` {
		t.Errorf("assembled arguments = %q, want %q", argsBuf.String(), `{"city":"SF"}`)
	}
	if !strings.Contains(all, `"finish_reason":"tool_calls"`) {
		t.Errorf("missing finish_reason tool_calls: %s", all)
	}
	if usage == nil || usage.Total != 13 {
		t.Errorf("usage = %+v, want total 13", usage)
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
