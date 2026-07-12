package openai_bedrock

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestTranslateRequest_Basic(t *testing.T) {
	in := `{"model":"m","max_tokens":100,"temperature":0.3,"top_p":0.9,"stream":true,
	        "messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`
	out, err := translateRequest([]byte(in))
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	r := gjson.ParseBytes(out)
	if r.Get("system.0.text").String() != "be brief" {
		t.Errorf("system mapping wrong: %s", out)
	}
	if r.Get("messages.0.role").String() != "user" || r.Get("messages.0.content.0.text").String() != "hi" {
		t.Errorf("messages mapping wrong: %s", out)
	}
	if r.Get("inferenceConfig.maxTokens").Int() != 100 || r.Get("inferenceConfig.temperature").Float() != 0.3 || r.Get("inferenceConfig.topP").Float() != 0.9 {
		t.Errorf("inferenceConfig mapping wrong: %s", out)
	}
	if !r.Get("stream").Bool() {
		t.Error("synthetic stream field should be true (stripped later by the Converse session)")
	}
	if r.Get("model").Exists() {
		t.Errorf("model should not appear in a Converse body (it's in the URL): %s", out)
	}
}

// TestTranslateRequest_ToolCallRoundTrip covers an assistant tool_calls
// message (arguments as a JSON string, per OpenAI's convention) becoming a
// toolUse block (input as a JSON object, per Converse's), and consecutive
// "tool" messages merging into one user message with multiple toolResult
// blocks -- Converse requires tool results to arrive as part of the next
// user turn, not a distinct role (see package doc comment).
func TestTranslateRequest_ToolCallRoundTrip(t *testing.T) {
	in := `{"model":"m","messages":[
	  {"role":"user","content":"weather in SF and NYC?"},
	  {"role":"assistant","content":null,"tool_calls":[
	    {"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}},
	    {"id":"call_2","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"NYC\"}"}}
	  ]},
	  {"role":"tool","tool_call_id":"call_1","content":"sunny"},
	  {"role":"tool","tool_call_id":"call_2","content":"rainy"}
	]}`
	out, err := translateRequest([]byte(in))
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	r := gjson.ParseBytes(out)
	if n := len(r.Get("messages").Array()); n != 3 {
		t.Fatalf("want 3 Converse messages (user, assistant-with-2-toolUse, user-with-2-toolResult), got %d: %s", n, out)
	}
	asst := r.Get("messages.1")
	if asst.Get("role").String() != "assistant" || len(asst.Get("content").Array()) != 2 {
		t.Fatalf("assistant message wrong: %s", asst.Raw)
	}
	if got := asst.Get("content.0.toolUse.input.city").String(); got != "SF" {
		t.Errorf("toolUse.input not a JSON object (city=%q): %s", got, asst.Raw)
	}
	toolResults := r.Get("messages.2")
	if toolResults.Get("role").String() != "user" || len(toolResults.Get("content").Array()) != 2 {
		t.Fatalf("merged tool results wrong (want 1 user message, 2 toolResult blocks): %s", toolResults.Raw)
	}
	if toolResults.Get("content.0.toolResult.toolUseId").String() != "call_1" ||
		toolResults.Get("content.0.toolResult.content.0.text").String() != "sunny" {
		t.Errorf("toolResult mapping wrong: %s", toolResults.Raw)
	}
}

func TestTranslateRequest_Tools(t *testing.T) {
	in := `{"model":"m","tools":[{"type":"function","function":{"name":"get_weather","description":"gets weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
	        "tool_choice":"required","messages":[{"role":"user","content":"weather in SF?"}]}`
	out, err := translateRequest([]byte(in))
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	r := gjson.ParseBytes(out)
	if r.Get("toolConfig.tools.0.toolSpec.name").String() != "get_weather" {
		t.Errorf("toolSpec.name wrong: %s", out)
	}
	if r.Get("toolConfig.tools.0.toolSpec.inputSchema.json.properties.city.type").String() != "string" {
		t.Errorf("inputSchema not carried over: %s", out)
	}
	if !r.Get("toolConfig.toolChoice.any").Exists() {
		t.Errorf(`tool_choice:"required" should map to {"any":{}}: %s`, out)
	}
}

func TestTranslateRequest_ToolChoiceNamed(t *testing.T) {
	in := `{"model":"m","tools":[{"type":"function","function":{"name":"get_weather","parameters":{}}}],
	        "tool_choice":{"type":"function","function":{"name":"get_weather"}},
	        "messages":[{"role":"user","content":"hi"}]}`
	out, err := translateRequest([]byte(in))
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	r := gjson.ParseBytes(out)
	if r.Get("toolConfig.toolChoice.tool.name").String() != "get_weather" {
		t.Errorf("named tool_choice mapping wrong: %s", out)
	}
}

func TestTranslateResponse_Text(t *testing.T) {
	in := []byte(`{"output":{"message":{"role":"assistant","content":[{"text":"hello"}]}},"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":2,"totalTokens":12}}`)
	out, usage := translateResponse(in)
	r := gjson.ParseBytes(out)
	if r.Get("choices.0.message.content").String() != "hello" {
		t.Errorf("content wrong: %s", out)
	}
	if r.Get("choices.0.finish_reason").String() != "stop" {
		t.Errorf("finish_reason wrong: %s", out)
	}
	if usage == nil || usage.Input != 10 || usage.Output != 2 || usage.Total != 12 {
		t.Errorf("usage wrong: %+v", usage)
	}
}

func TestTranslateResponse_ToolUse(t *testing.T) {
	in := []byte(`{"output":{"message":{"role":"assistant","content":[{"toolUse":{"toolUseId":"tu_1","name":"get_weather","input":{"city":"SF"}}}]}},"stopReason":"tool_use","usage":{"inputTokens":10,"outputTokens":2,"totalTokens":12}}`)
	out, _ := translateResponse(in)
	r := gjson.ParseBytes(out)
	if r.Get("choices.0.message.content").Type != gjson.Null {
		t.Errorf("content should be null when the turn is tool_calls-only: %s", out)
	}
	if r.Get("choices.0.message.tool_calls.0.function.name").String() != "get_weather" {
		t.Errorf("tool_calls mapping wrong: %s", out)
	}
	if r.Get("choices.0.message.tool_calls.0.function.arguments").String() != `{"city":"SF"}` {
		t.Errorf("tool_calls arguments should be a JSON string of the input object: %s", out)
	}
	if r.Get("choices.0.finish_reason").String() != "tool_calls" {
		t.Errorf("finish_reason wrong: %s", out)
	}
}

func TestTranslateResponse_Reasoning(t *testing.T) {
	in := []byte(`{"output":{"message":{"role":"assistant","content":[
	  {"reasoningContent":{"reasoningText":{"text":"thinking...","signature":"sig"}}},
	  {"text":"the answer"}
	]}},"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":2,"totalTokens":12}}`)
	out, _ := translateResponse(in)
	r := gjson.ParseBytes(out)
	if r.Get("choices.0.message.reasoning_content").String() != "thinking..." {
		t.Errorf("reasoning_content wrong: %s", out)
	}
	if r.Get("choices.0.message.content").String() != "the answer" {
		t.Errorf("content wrong: %s", out)
	}
}

// TestStreamToolCallIndexIsZeroBased: when a Converse stream emits a text
// block (contentBlockIndex 0) before a tool-use block (contentBlockIndex 1),
// the OpenAI tool_calls[].index sent to the client must be 0 (a 0-based
// ordinal over tool calls), not the raw Converse contentBlockIndex of 1 --
// otherwise a client that array-indexes tool_calls places the first call at
// slot 1.
func TestStreamToolCallIndexIsZeroBased(t *testing.T) {
	h := New().NewResponseHandler()
	stream := "event: messageStart\ndata: {\"role\":\"assistant\"}\n\n" +
		"event: contentBlockStart\ndata: {\"contentBlockIndex\":0,\"start\":{}}\n\n" +
		"event: contentBlockDelta\ndata: {\"contentBlockIndex\":0,\"delta\":{\"text\":\"checking\"}}\n\n" +
		"event: contentBlockStart\ndata: {\"contentBlockIndex\":1,\"start\":{\"toolUse\":{\"toolUseId\":\"tu_1\",\"name\":\"get_weather\"}}}\n\n" +
		"event: contentBlockDelta\ndata: {\"contentBlockIndex\":1,\"delta\":{\"toolUse\":{\"input\":\"{\\\"city\\\":\\\"SF\\\"}\"}}}\n\n"

	out, _ := h.Feed([]byte(stream))
	rest, _, _ := h.Flush()
	all := string(out) + string(rest)

	// Find the tool_calls chunk that carries the tool id (the "start" chunk).
	var idx int64 = -1
	for _, line := range strings.Split(all, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		tc := gjson.Get(payload, "choices.0.delta.tool_calls.0")
		if tc.Get("id").String() == "tu_1" {
			idx = tc.Get("index").Int()
		}
	}
	if idx != 0 {
		t.Errorf("first tool call index = %d, want 0 (0-based over tool calls, not Converse block index)\nstream out:\n%s", idx, all)
	}
}

func TestMapFinishReason(t *testing.T) {
	cases := map[string]string{
		"end_turn": "stop", "stop_sequence": "stop", "": "stop",
		"tool_use": "tool_calls", "max_tokens": "length",
		"content_filtered": "content_filter", "guardrail_intervened": "content_filter",
		"something_new": "stop",
	}
	for sr, want := range cases {
		if got := mapFinishReason(sr); got != want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", sr, got, want)
		}
	}
}
