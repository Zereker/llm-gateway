package anthropic_openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestMapFinishReason_Completeness covers every documented OpenAI
// finish_reason value, including the deprecated function_call, so it can't
// silently collapse into "end_turn" and hide a pending tool call.
func TestMapFinishReason_Completeness(t *testing.T) {
	anthropicValidStopReasons := map[string]bool{
		"end_turn": true, "max_tokens": true, "stop_sequence": true,
		"tool_use": true, "refusal": true, "pause_turn": true,
	}
	for in, want := range map[string]string{
		"stop":   "end_turn",
		"length": "max_tokens",
		// content_filter means the upstream's moderation blocked the content —
		// map it to Anthropic's "refusal" (the round-trip inverse of
		// openai_anthropic mapping Anthropic "refusal" -> OpenAI "content_filter").
		// "stop_sequence" would falsely tell the client a configured stop string
		// matched.
		"content_filter": "refusal",
		"tool_calls":     "tool_use",
		"function_call":  "tool_use",
		"":               "end_turn",
	} {
		got := mapFinishReason(in)
		if got != want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", in, got, want)
		}
		if !anthropicValidStopReasons[got] {
			t.Errorf("mapFinishReason(%q) = %q, not a valid Anthropic stop_reason", in, got)
		}
	}
}

// TestIsOpenAIError_StructuralNotSubstring: error detection must key off a
// structural top-level "error" object, not a naive substring scan — otherwise
// a successful response that merely contains the bytes "error" (a compat
// backend's "error":null parity field, or content that happens to be the word
// "error") gets misdetected, returning the raw untranslated body to the client
// AND zeroing its usage/billing.
func TestIsOpenAIError_StructuralNotSubstring(t *testing.T) {
	cases := []struct {
		body string
		want bool
		note string
	}{
		{`{"error":{"message":"bad key","type":"invalid_request_error"}}`, true, "real top-level error object"},
		{`{"id":"chatcmpl-x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}],"error":null}`, false, "success with error:null (vLLM/LiteLLM API parity) must not be misdetected"},
		{`{"id":"chatcmpl-x","choices":[{"message":{"role":"assistant","content":"error"}}]}`, false, "success whose content is literally \"error\""},
	}
	for _, tc := range cases {
		if got := isOpenAIError([]byte(tc.body)); got != tc.want {
			t.Errorf("isOpenAIError(%s) = %v, want %v (%s)", tc.body, got, tc.want, tc.note)
		}
	}
}

// parseSSEData extracts the JSON payload of every `data:` line from an Anthropic
// SSE stream, decoding each into a generic map for assertions.
func parseSSEData(t *testing.T, out string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("invalid SSE data JSON %q: %v", payload, err)
		}
		events = append(events, m)
	}
	return events
}

// A standard Anthropic SDK request sends content (and system) as an array of
// content blocks. translateRequest must accept it, not reject it with a parse
// error.
// TestTranslateRequest_ImageBlock covers Anthropic image content -> OpenAI
// image_url content parts, both base64 and url source forms. The base64
// payload is a real captured image (a tiny PNG) from
// simonw/llm-anthropic's test_image_prompt.yaml cassette (Apache 2.0).
func TestTranslateRequest_ImageBlock(t *testing.T) {
	const realPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAKYAAAEaAgMAAADmmcReAAAACVBMVEX///8A/wD+AQASdAFKAAAAR0lEQVR42u3YMREAMAjAwC5d6q8mUYkEVuA+8yvIkVr0oghFURRFURRFURRFUdRCkSRJM7u/CEVRFEVRFEVRFEXRpdQXkcaVBRUPn8UJn6QAAAAASUVORK5CYII="

	t.Run("base64", func(t *testing.T) {
		body := []byte(`{"model":"claude-x","max_tokens":100,"messages":[
			{"role":"user","content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + realPNGBase64 + `"}},
				{"type":"text","text":"Describe image in three words"}
			]}
		]}`)
		out, err := translateRequest(body)
		if err != nil {
			t.Fatalf("translateRequest error: %v", err)
		}
		var got struct {
			Messages []struct {
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("output not valid OpenAI request: %v\n%s", err, out)
		}
		if len(got.Messages) != 1 || len(got.Messages[0].Content) != 2 {
			t.Fatalf("want 1 message with 2 parts, got: %s", out)
		}
		wantURL := "data:image/png;base64," + realPNGBase64
		if img := got.Messages[0].Content[0]; img.Type != "image_url" || img.ImageURL.URL != wantURL {
			t.Errorf("image_url part wrong: %+v", img)
		}
		if txt := got.Messages[0].Content[1]; txt.Type != "text" || txt.Text != "Describe image in three words" {
			t.Errorf("text part wrong: %+v", txt)
		}
	})

	t.Run("url", func(t *testing.T) {
		body := []byte(`{"model":"claude-x","max_tokens":100,"messages":[
			{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/cat.png"}}]}
		]}`)
		out, err := translateRequest(body)
		if err != nil {
			t.Fatalf("translateRequest error: %v", err)
		}
		var got struct {
			Messages []struct {
				Content []struct {
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("output not valid OpenAI request: %v\n%s", err, out)
		}
		if got := got.Messages[0].Content[0].ImageURL.URL; got != "https://example.com/cat.png" {
			t.Errorf("image_url = %q, want passthrough URL", got)
		}
	})
}

func TestTranslateRequest_ArrayContentAndSystem(t *testing.T) {
	body := []byte(`{
		"model": "claude-x",
		"system": [{"type":"text","text":"be terse"}],
		"max_tokens": 100,
		"messages": [
			{"role":"user","content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}
		]
	}`)

	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("array content/system must translate, got error: %v", err)
	}

	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid OpenAI request: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("want system+user messages, got %d", len(got.Messages))
	}
	if got.Messages[0].Role != "system" || got.Messages[0].Content != "be terse" {
		t.Errorf("system block not flattened: %+v", got.Messages[0])
	}
	if got.Messages[1].Content != "hello world" {
		t.Errorf("content blocks not concatenated: %q", got.Messages[1].Content)
	}
}

// String content must still work (regression guard).
func TestTranslateRequest_StringContent(t *testing.T) {
	body := []byte(`{"model":"claude-x","system":"sys","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("string content must translate: %v", err)
	}
	if !strings.Contains(string(out), `"content":"hi"`) {
		t.Errorf("string content lost: %s", out)
	}
}

// Request translation: tools + assistant tool_use + user tool_result must map to
// the OpenAI function-calling shapes.
// TestTranslateRequest_ToolStrict: Anthropic's tool-level "strict" flag
// (verified against a real captured request/response pair,
// langchain-ai/langchain's official langchain-anthropic package, Apache 2.0,
// tests/cassettes/test_strict_tool_use.yaml.gz) must carry over to OpenAI's
// tool definition verbatim — same field name.
func TestTranslateRequest_ToolStrict(t *testing.T) {
	body := []byte(`{"model":"claude-x","max_tokens":100,
		"tools":[{"name":"get_weather","description":"Get the weather at a location.",
		"input_schema":{"type":"object","properties":{"location":{"type":"string"},"unit":{"type":"string","enum":["C","F"]}},"required":["location","unit"],"additionalProperties":false},
		"strict":true}],
		"messages":[{"role":"user","content":"weather in Boston, in Celsius?"}]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translateRequest error: %v", err)
	}
	r := gjson.ParseBytes(out)
	if !r.Get("tools.0.function.strict").Bool() {
		t.Errorf("strict flag dropped: %s", out)
	}
}

func TestTranslateRequest_Tools(t *testing.T) {
	body := []byte(`{
		"model": "claude-x",
		"max_tokens": 100,
		"tools": [
			{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}},
			{"name":"noop"}
		],
		"tool_choice": {"type":"auto"},
		"messages": [
			{"role":"user","content":"weather in SF?"},
			{"role":"assistant","content":[
				{"type":"text","text":"let me check"},
				{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"SF"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1","content":"sunny"},
				{"type":"text","text":"thanks"}
			]}
		]
	}`)

	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translateRequest error: %v", err)
	}

	var got struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
		Messages   []struct {
			Role       string  `json:"role"`
			Content    *string `json:"content"`
			ToolCallID string  `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid OpenAI request: %v\n%s", err, out)
	}

	// tools -> function shape with parameters
	if len(got.Tools) != 2 {
		t.Fatalf("want 2 tools, got %d", len(got.Tools))
	}
	if got.Tools[0].Type != "function" || got.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tool[0] shape wrong: %+v", got.Tools[0])
	}
	if !strings.Contains(string(got.Tools[0].Function.Parameters), `"city"`) {
		t.Errorf("input_schema not carried into parameters: %s", got.Tools[0].Function.Parameters)
	}
	// empty input_schema -> {"type":"object"}
	if strings.TrimSpace(string(got.Tools[1].Function.Parameters)) != `{"type":"object"}` {
		t.Errorf("empty input_schema should default to object: %s", got.Tools[1].Function.Parameters)
	}
	if strings.TrimSpace(string(got.ToolChoice)) != `"auto"` {
		t.Errorf("tool_choice auto mapping wrong: %s", got.ToolChoice)
	}

	// messages: user, assistant(w/ tool_calls), tool, user(text)
	if len(got.Messages) != 4 {
		t.Fatalf("want 4 messages, got %d: %s", len(got.Messages), out)
	}
	asst := got.Messages[1]
	if asst.Role != "assistant" {
		t.Fatalf("messages[1] role = %q, want assistant", asst.Role)
	}
	if asst.Content == nil || *asst.Content != "let me check" {
		t.Errorf("assistant content wrong: %v", asst.Content)
	}
	if len(asst.ToolCalls) != 1 {
		t.Fatalf("want 1 tool_call, got %d", len(asst.ToolCalls))
	}
	tc := asst.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Errorf("tool_call shape wrong: %+v", tc)
	}
	// arguments must be a JSON string of the input object
	var argObj map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &argObj); err != nil {
		t.Fatalf("arguments not a JSON string: %q", tc.Function.Arguments)
	}
	if argObj["city"] != "SF" {
		t.Errorf("arguments content wrong: %v", argObj)
	}
	// tool message first, then user text
	toolMsg := got.Messages[2]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_1" || toolMsg.Content == nil || *toolMsg.Content != "sunny" {
		t.Errorf("tool result message wrong: %+v", toolMsg)
	}
	userTxt := got.Messages[3]
	if userTxt.Role != "user" || userTxt.Content == nil || *userTxt.Content != "thanks" {
		t.Errorf("trailing user text message wrong: %+v", userTxt)
	}
}

// tool_result content given as a block array must be stringified to its text.
func TestTranslateRequest_ToolResultBlockArray(t *testing.T) {
	body := []byte(`{
		"model":"claude-x","max_tokens":10,
		"messages":[
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_9","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}
			]}
		]
	}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translateRequest error: %v", err)
	}
	if !strings.Contains(string(out), `"tool_call_id":"call_9"`) || !strings.Contains(string(out), `"content":"ab"`) {
		t.Errorf("tool_result block array not stringified: %s", out)
	}
}

func TestTranslateRequest_ToolChoice(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"type":"auto"}`, `"auto"`},
		{`{"type":"any"}`, `"required"`},
		{`{"type":"none"}`, `"none"`},
		{`{"type":"tool","name":"foo"}`, `{"type":"function","function":{"name":"foo"}}`},
	}
	for _, c := range cases {
		body := []byte(`{"model":"m","max_tokens":10,"tool_choice":` + c.in + `,"messages":[{"role":"user","content":"hi"}]}`)
		out, err := translateRequest(body)
		if err != nil {
			t.Fatalf("translateRequest(%s) error: %v", c.in, err)
		}
		var got struct {
			ToolChoice json.RawMessage `json:"tool_choice"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// compact both sides for comparison
		var want, have any
		if err := json.Unmarshal([]byte(c.want), &want); err != nil {
			t.Fatalf("unmarshal want: %v", err)
		}
		if err := json.Unmarshal(got.ToolChoice, &have); err != nil {
			t.Fatalf("unmarshal tool_choice: %v", err)
		}
		wb, _ := json.Marshal(want)
		hb, _ := json.Marshal(have)
		if string(wb) != string(hb) {
			t.Errorf("tool_choice %s -> %s, want %s", c.in, hb, wb)
		}
	}
}

// disable_parallel_tool_use:true must invert into OpenAI's parallel_tool_calls:false,
// regardless of what tool_choice.type it's attached to.
func TestTranslateRequest_DisableParallelToolUse(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":10,` +
		`"tool_choice":{"type":"auto","disable_parallel_tool_use":true},` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translateRequest error: %v", err)
	}
	var got struct {
		ParallelToolCalls *bool `json:"parallel_tool_calls"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ParallelToolCalls == nil || *got.ParallelToolCalls {
		t.Errorf("parallel_tool_calls = %v, want false", got.ParallelToolCalls)
	}
}

// Without disable_parallel_tool_use, parallel_tool_calls must stay omitted
// (nil), not be forced to true — OpenAI's own default should apply.
func TestTranslateRequest_ParallelToolCallsOmittedByDefault(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":10,"tool_choice":{"type":"auto"},` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translateRequest error: %v", err)
	}
	var got struct {
		ParallelToolCalls *bool `json:"parallel_tool_calls"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ParallelToolCalls != nil {
		t.Errorf("parallel_tool_calls = %v, want omitted", *got.ParallelToolCalls)
	}
}

// Non-streaming response with content + tool_calls -> text block + tool_use blocks.
func TestTranslateResponse_ToolCalls(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-abc","model":"gpt-4o",
		"choices":[{"index":0,"message":{"role":"assistant","content":"sure","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}
	}`)

	out, err := translateResponse(body, "fallback")
	if err != nil {
		t.Fatalf("translateResponse error: %v", err)
	}
	var got struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("bad anthropic response: %v\n%s", err, out)
	}
	if got.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", got.StopReason)
	}
	if len(got.Content) != 2 {
		t.Fatalf("want text+tool_use blocks, got %d: %s", len(got.Content), out)
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "sure" {
		t.Errorf("content[0] wrong: %+v", got.Content[0])
	}
	if got.Content[1].Type != "tool_use" || got.Content[1].ID != "call_1" || got.Content[1].Name != "get_weather" {
		t.Errorf("content[1] wrong: %+v", got.Content[1])
	}
	var input map[string]any
	if err := json.Unmarshal(got.Content[1].Input, &input); err != nil {
		t.Fatalf("input not an object: %s", got.Content[1].Input)
	}
	if input["city"] != "SF" {
		t.Errorf("input content wrong: %v", input)
	}
}

// Non-streaming response with tool_calls but no content -> only tool_use block(s).
func TestTranslateResponse_ToolCallsNoContent(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"not json"}}]},"finish_reason":"tool_calls"}]}`)
	out, err := translateResponse(body, "m")
	if err != nil {
		t.Fatalf("translateResponse error: %v", err)
	}
	var got struct {
		Content []struct {
			Type  string          `json:"type"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Content) != 1 || got.Content[0].Type != "tool_use" {
		t.Fatalf("want single tool_use block, got %s", out)
	}
	// invalid arguments JSON must fall back to {}
	if strings.TrimSpace(string(got.Content[0].Input)) != `{}` {
		t.Errorf("invalid arguments should yield empty object input: %s", got.Content[0].Input)
	}
}

// Plain-text non-streaming response regression: single text block, end_turn.
func TestTranslateResponse_PlainText(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	out, err := translateResponse(body, "m")
	if err != nil {
		t.Fatalf("translateResponse error: %v", err)
	}
	if !strings.Contains(string(out), `"type":"text"`) || !strings.Contains(string(out), `"text":"hello"`) {
		t.Errorf("plain text block lost: %s", out)
	}
	if !strings.Contains(string(out), `"stop_reason":"end_turn"`) {
		t.Errorf("stop_reason wrong: %s", out)
	}
}

// feedStream runs an OpenAI SSE string through a fresh handler and returns the
// concatenated Anthropic output (Feed emissions + Flush fallback).
func feedStream(t *testing.T, sse string) string {
	t.Helper()
	h := New().(anthropicOpenAI).NewResponseHandler()
	var sb strings.Builder
	out, err := h.Feed([]byte(sse))
	if err != nil {
		t.Fatalf("Feed error: %v", err)
	}
	sb.Write(out)
	tail, _, err := h.Flush()
	if err != nil {
		t.Fatalf("Flush error: %v", err)
	}
	sb.Write(tail)
	return sb.String()
}

// Streaming: a single tool call streamed as header + argument fragments.
func TestStreaming_ToolCall(t *testing.T) {
	sse := `data: {"id":"chatcmpl-1","model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ci"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"SF\"}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]

`
	out := feedStream(t, sse)
	events := parseSSEData(t, out)

	var sawStart, sawStop, sawMsgDelta, sawMsgStop bool
	var args strings.Builder
	for _, e := range events {
		switch e["type"] {
		case "content_block_start":
			cb, _ := e["content_block"].(map[string]any)
			if cb["type"] == "tool_use" {
				sawStart = true
				if idx, _ := e["index"].(float64); idx != 0 {
					t.Errorf("tool block index = %v, want 0", e["index"])
				}
				if cb["id"] != "call_1" || cb["name"] != "get_weather" {
					t.Errorf("tool_use start block wrong: %v", cb)
				}
			}
		case "content_block_delta":
			d, _ := e["delta"].(map[string]any)
			if d["type"] == "input_json_delta" {
				args.WriteString(d["partial_json"].(string))
			}
		case "content_block_stop":
			sawStop = true
		case "message_delta":
			d, _ := e["delta"].(map[string]any)
			if d["stop_reason"] == "tool_use" {
				sawMsgDelta = true
			}
		case "message_stop":
			sawMsgStop = true
		}
	}
	if !sawStart {
		t.Error("missing content_block_start tool_use")
	}
	if args.String() != `{"city":"SF"}` {
		t.Errorf("reassembled arguments = %q, want {\"city\":\"SF\"}", args.String())
	}
	if !sawStop {
		t.Error("missing content_block_stop")
	}
	if !sawMsgDelta {
		t.Error("missing message_delta with stop_reason tool_use")
	}
	if !sawMsgStop {
		t.Error("missing message_stop")
	}
}

// Streaming: text first (index 0) then a tool call (index 1); the text block must
// be stopped before the tool block starts.
func TestStreaming_TextThenTool(t *testing.T) {
	sse := `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi "},"finish_reason":null}]}

data: {"id":"c","choices":[{"index":0,"delta":{"content":"there"},"finish_reason":null}]}

data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}

data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	out := feedStream(t, sse)
	events := parseSSEData(t, out)

	// Collect ordered (type, index, blockType) tuples.
	type ev struct {
		typ, blockType string
		index          float64
	}
	var seq []ev
	for _, e := range events {
		idx, _ := e["index"].(float64)
		bt := ""
		if cb, ok := e["content_block"].(map[string]any); ok {
			bt, _ = cb["type"].(string)
		}
		seq = append(seq, ev{typ: e["type"].(string), blockType: bt, index: idx})
	}

	// Expect: text start@0, then before tool start@1 there must be a stop@0.
	var textStartIdx, textStopIdx, toolStartIdx = -1, -1, -1
	for i, e := range seq {
		switch {
		case e.typ == "content_block_start" && e.blockType == "text" && e.index == 0:
			textStartIdx = i
		case e.typ == "content_block_stop" && e.index == 0 && textStopIdx == -1:
			textStopIdx = i
		case e.typ == "content_block_start" && e.blockType == "tool_use" && e.index == 1:
			toolStartIdx = i
		}
	}
	if textStartIdx == -1 {
		t.Fatal("no text content_block_start at index 0")
	}
	if toolStartIdx == -1 {
		t.Fatal("no tool_use content_block_start at index 1")
	}
	if textStopIdx == -1 || textStopIdx > toolStartIdx {
		t.Errorf("text block (index 0) must be stopped before tool block starts; stop@%d tool@%d", textStopIdx, toolStartIdx)
	}
}

// Streaming: two parallel tool calls get Anthropic indices 0 and 1.
func TestStreaming_TwoToolCalls(t *testing.T) {
	sse := `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f1","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}

data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"f2","arguments":"{\"b\":2}"}}]},"finish_reason":null}]}

data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	out := feedStream(t, sse)
	events := parseSSEData(t, out)

	starts := map[float64]string{}
	stops := 0
	for _, e := range events {
		switch e["type"] {
		case "content_block_start":
			if cb, ok := e["content_block"].(map[string]any); ok && cb["type"] == "tool_use" {
				idx, _ := e["index"].(float64)
				starts[idx] = cb["id"].(string)
			}
		case "content_block_stop":
			stops++
		}
	}
	if starts[0] != "call_1" || starts[1] != "call_2" {
		t.Errorf("tool block indices wrong: %v", starts)
	}
	// Two blocks -> two stops (first tool stopped when second starts, last on close).
	if stops != 2 {
		t.Errorf("want 2 content_block_stop, got %d", stops)
	}
}

// Streaming plain-text regression: text block open/delta/stop, stop_reason end_turn.
func TestStreaming_PlainText(t *testing.T) {
	sse := `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}

data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	out := feedStream(t, sse)
	if !strings.Contains(out, `"type":"content_block_start"`) || !strings.Contains(out, `"text_delta"`) {
		t.Errorf("text streaming events missing: %s", out)
	}
	if !strings.Contains(out, `"type":"content_block_stop"`) {
		t.Errorf("missing content_block_stop: %s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Errorf("stop_reason wrong: %s", out)
	}
}
