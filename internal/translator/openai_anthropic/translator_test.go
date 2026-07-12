package openai_anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestMapStopReason_Completeness covers every documented Anthropic stop_reason
// value (https://platform.claude.com/docs/en/build-with-claude/handling-stop-reasons)
// so a refusal or a mid-turn pause can't silently collapse into a generic
// "stop" that looks like a normal completion.
func TestMapStopReason_Completeness(t *testing.T) {
	openAIValidFinishReasons := map[string]bool{
		"stop": true, "length": true, "tool_calls": true, "content_filter": true,
	}
	for in, want := range map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"refusal":       "content_filter",
		"pause_turn":    "stop",
		"":              "stop",
	} {
		got := mapStopReason(in)
		if got != want {
			t.Errorf("mapStopReason(%q) = %q, want %q", in, got, want)
		}
		if !openAIValidFinishReasons[got] {
			t.Errorf("mapStopReason(%q) = %q, not a valid OpenAI finish_reason", in, got)
		}
	}
}

// OpenAI vision / multi-part requests send content as an array of parts.
// translateRequest must accept it, not reject it with a parse error.
// TestTranslateRequest_ImageURL covers OpenAI vision content -> Anthropic
// image blocks, both the data: URI form (base64 decoded into
// source.media_type/data) and a plain https URL (passed through as
// source.type=url). The base64 payload is a real captured image (a tiny PNG)
// from simonw/llm-anthropic's test_image_prompt.yaml cassette (Apache 2.0),
// not a synthetic string, so the exact media_type/data split is exercised
// against real vision-request field shapes.
func TestTranslateRequest_ImageURL(t *testing.T) {
	const realPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAKYAAAEaAgMAAADmmcReAAAACVBMVEX///8A/wD+AQASdAFKAAAAR0lEQVR42u3YMREAMAjAwC5d6q8mUYkEVuA+8yvIkVr0oghFURRFURRFURRFUdRCkSRJM7u/CEVRFEVRFEVRFEXRpdQXkcaVBRUPn8UJn6QAAAAASUVORK5CYII="

	t.Run("data_uri", func(t *testing.T) {
		body := []byte(`{"model":"gpt-x","max_tokens":50,"messages":[
			{"role":"user","content":[
				{"type":"image_url","image_url":{"url":"data:image/png;base64,` + realPNGBase64 + `"}},
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
					Type   string `json:"type"`
					Text   string `json:"text"`
					Source struct {
						Type      string `json:"type"`
						MediaType string `json:"media_type"`
						Data      string `json:"data"`
					} `json:"source"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("output not valid Anthropic request: %v\n%s", err, out)
		}
		if len(got.Messages) != 1 || len(got.Messages[0].Content) != 2 {
			t.Fatalf("want 1 message with 2 blocks, got: %s", out)
		}
		img := got.Messages[0].Content[0]
		if img.Type != "image" || img.Source.Type != "base64" || img.Source.MediaType != "image/png" || img.Source.Data != realPNGBase64 {
			t.Errorf("image block wrong: %+v", img)
		}
		if txt := got.Messages[0].Content[1]; txt.Type != "text" || txt.Text != "Describe image in three words" {
			t.Errorf("text block wrong: %+v", txt)
		}
	})

	t.Run("https_url_passthrough", func(t *testing.T) {
		body := []byte(`{"model":"gpt-x","max_tokens":50,"messages":[
			{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}
		]}`)
		out, err := translateRequest(body)
		if err != nil {
			t.Fatalf("translateRequest error: %v", err)
		}
		var got struct {
			Messages []struct {
				Content []struct {
					Source struct {
						Type string `json:"type"`
						URL  string `json:"url"`
					} `json:"source"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("output not valid Anthropic request: %v\n%s", err, out)
		}
		src := got.Messages[0].Content[0].Source
		if src.Type != "url" || src.URL != "https://example.com/cat.png" {
			t.Errorf("url image block wrong: %+v", src)
		}
	})
}

func TestTranslateRequest_ArrayContent(t *testing.T) {
	body := []byte(`{
		"model": "gpt-x",
		"max_tokens": 50,
		"messages": [
			{"role":"system","content":[{"type":"text","text":"be brief"}]},
			{"role":"user","content":[{"type":"text","text":"foo "},{"type":"text","text":"bar"}]}
		]
	}`)

	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("array content must translate, got error: %v", err)
	}

	var got struct {
		System   string `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid Anthropic request: %v", err)
	}
	if got.System != "be brief" {
		t.Errorf("system parts not flattened: %q", got.System)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "foo bar" {
		t.Errorf("content parts not concatenated: %+v", got.Messages)
	}
}

// anthReqBlock is a decoded Anthropic content block (request or response side).
type anthReqBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   string          `json:"content"`
}

// decodeAnthReq unmarshals a translated Anthropic request for assertions. Each
// message Content is left raw so a test can decode it as a string OR a block
// array depending on the message.
type anthReq struct {
	Tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
	Messages   []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// TestTranslateRequest_Tools covers tool definitions, an assistant tool_calls
// message, and (merged) tool-result messages on the request path.
func TestTranslateRequest_Tools(t *testing.T) {
	body := []byte(`{
		"model": "gpt-x",
		"tools": [
			{"type":"function","function":{"name":"get_weather","description":"gets weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}},
			{"type":"function","function":{"name":"noparams"}}
		],
		"messages": [
			{"role":"user","content":"weather in SF and NY?"},
			{"role":"assistant","content":"sure","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"sunny"},
			{"role":"tool","tool_call_id":"call_2","content":"rainy"}
		]
	}`)

	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got anthReq
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("bad anthropic body: %v", err)
	}

	// tools → input_schema; missing parameters defaults to {"type":"object"}.
	if len(got.Tools) != 2 {
		t.Fatalf("want 2 tools, got %d", len(got.Tools))
	}
	if got.Tools[0].Name != "get_weather" || got.Tools[0].Description != "gets weather" {
		t.Errorf("tool0 = %+v", got.Tools[0])
	}
	if !strings.Contains(string(got.Tools[0].InputSchema), `"properties"`) {
		t.Errorf("tool0 input_schema not carried: %s", got.Tools[0].InputSchema)
	}
	if strings.TrimSpace(string(got.Tools[1].InputSchema)) != `{"type":"object"}` {
		t.Errorf("tool1 empty schema not defaulted: %s", got.Tools[1].InputSchema)
	}

	// messages: user, assistant(tool_use), merged tool_result user turn.
	if len(got.Messages) != 3 {
		t.Fatalf("want 3 messages (merged tool results), got %d: %s", len(got.Messages), out)
	}

	// assistant message → content array with a text block + tool_use block.
	if got.Messages[1].Role != "assistant" {
		t.Fatalf("msg1 role = %q", got.Messages[1].Role)
	}
	var asstBlocks []anthReqBlock
	if err := json.Unmarshal(got.Messages[1].Content, &asstBlocks); err != nil {
		t.Fatalf("assistant content not a block array: %v (%s)", err, got.Messages[1].Content)
	}
	if len(asstBlocks) != 2 || asstBlocks[0].Type != "text" || asstBlocks[0].Text != "sure" {
		t.Fatalf("assistant blocks = %+v", asstBlocks)
	}
	if asstBlocks[1].Type != "tool_use" || asstBlocks[1].ID != "call_1" || asstBlocks[1].Name != "get_weather" {
		t.Errorf("tool_use block = %+v", asstBlocks[1])
	}
	// input must be a parsed object, not a JSON string.
	var input map[string]any
	if err := json.Unmarshal(asstBlocks[1].Input, &input); err != nil {
		t.Fatalf("tool_use input not an object: %v (%s)", err, asstBlocks[1].Input)
	}
	if input["city"] != "SF" {
		t.Errorf("tool_use input = %v", input)
	}

	// merged tool results → single user message with two tool_result blocks.
	if got.Messages[2].Role != "user" {
		t.Fatalf("merged tool msg role = %q", got.Messages[2].Role)
	}
	var trBlocks []anthReqBlock
	if err := json.Unmarshal(got.Messages[2].Content, &trBlocks); err != nil {
		t.Fatalf("tool_result content not a block array: %v", err)
	}
	if len(trBlocks) != 2 {
		t.Fatalf("want 2 merged tool_result blocks, got %d", len(trBlocks))
	}
	if trBlocks[0].Type != "tool_result" || trBlocks[0].ToolUseID != "call_1" || trBlocks[0].Content != "sunny" {
		t.Errorf("tool_result0 = %+v", trBlocks[0])
	}
	if trBlocks[1].ToolUseID != "call_2" || trBlocks[1].Content != "rainy" {
		t.Errorf("tool_result1 = %+v", trBlocks[1])
	}

	// plain user message still uses the simple string content shape.
	var userStr string
	if err := json.Unmarshal(got.Messages[0].Content, &userStr); err != nil {
		t.Fatalf("plain user content not a string: %v (%s)", err, got.Messages[0].Content)
	}
	if userStr != "weather in SF and NY?" {
		t.Errorf("user content = %q", userStr)
	}
}

// TestTranslateRequest_ToolStrict: OpenAI's tool-level "strict" flag must
// carry over to Anthropic's tool definition verbatim — Anthropic's schema
// accepts the same field name, verified against a real captured request
// (langchain-ai/langchain's official langchain-anthropic package, Apache 2.0,
// tests/cassettes/test_strict_tool_use.yaml.gz).
func TestTranslateRequest_ToolStrict(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"weather?"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"Get the weather at a location.",
		"parameters":{"type":"object","properties":{"location":{"type":"string"},"unit":{"type":"string","enum":["C","F"]}},"required":["location","unit"],"additionalProperties":false},
		"strict":true}}]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	r := gjson.ParseBytes(out)
	if !r.Get("tools.0.strict").Bool() {
		t.Errorf("strict flag dropped: %s", out)
	}
	if !r.Get("tools.0.input_schema.additionalProperties").Exists() || r.Get("tools.0.input_schema.additionalProperties").Bool() {
		t.Errorf("additionalProperties:false dropped from input_schema: %s", out)
	}
}

// TestTranslateRequest_BadToolArgs: unparsable function.arguments → input:{}.
func TestTranslateRequest_BadToolArgs(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":"","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"x","arguments":"not json"}}
		]}
	]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got anthReq
	_ = json.Unmarshal(out, &got)
	var blocks []anthReqBlock
	if err := json.Unmarshal(got.Messages[0].Content, &blocks); err != nil {
		t.Fatalf("content not array: %v", err)
	}
	// content is empty → no leading text block, just the tool_use block.
	if len(blocks) != 1 || blocks[0].Type != "tool_use" {
		t.Fatalf("blocks = %+v", blocks)
	}
	if strings.TrimSpace(string(blocks[0].Input)) != `{}` {
		t.Errorf("bad args should degrade to {}, got %s", blocks[0].Input)
	}
}

// TestTranslateRequest_ToolChoice covers the four tool_choice forms.
func TestTranslateRequest_ToolChoice(t *testing.T) {
	cases := []struct {
		name   string
		choice string
		want   string // "" means tool_choice omitted
	}{
		{"auto", `"auto"`, `{"type":"auto"}`},
		{"required", `"required"`, `{"type":"any"}`},
		{"object", `{"type":"function","function":{"name":"foo"}}`, `{"type":"tool","name":"foo"}`},
		// Anthropic has an explicit "none" tool_choice type; omitting it (the
		// old behavior) meant a client's "force no more tool calls this turn"
		// instruction silently became "auto" upstream.
		{"none", `"none"`, `{"type":"none"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"m","tool_choice":` + tc.choice +
				`,"tools":[{"type":"function","function":{"name":"foo"}}],"messages":[{"role":"user","content":"hi"}]}`)
			out, err := translateRequest(body)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			var got anthReq
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("bad body: %v", err)
			}
			// tools are always sent regardless of tool_choice.
			if len(got.Tools) != 1 {
				t.Errorf("tools should still be sent, got %d", len(got.Tools))
			}
			if tc.want == "" {
				if len(got.ToolChoice) != 0 {
					t.Errorf("want tool_choice omitted, got %s", got.ToolChoice)
				}
				return
			}
			if got.ToolChoice == nil {
				t.Fatalf("tool_choice missing, want %s", tc.want)
			}
			var a, b any
			_ = json.Unmarshal(got.ToolChoice, &a)
			_ = json.Unmarshal([]byte(tc.want), &b)
			if ja, _ := json.Marshal(a); string(ja) != mustCanon(tc.want) {
				t.Errorf("tool_choice = %s, want %s", got.ToolChoice, tc.want)
			}
		})
	}
}

// TestTranslateRequest_ParallelToolCallsFalse: OpenAI's parallel_tool_calls:false
// must invert into Anthropic's tool_choice.disable_parallel_tool_use:true, even
// when the client didn't send a tool_choice at all (a bare "type":"auto" object
// is synthesized so the flag has somewhere to live).
func TestTranslateRequest_ParallelToolCallsFalse(t *testing.T) {
	body := []byte(`{"model":"m","parallel_tool_calls":false,` +
		`"tools":[{"type":"function","function":{"name":"foo"}}],"messages":[{"role":"user","content":"hi"}]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got struct {
		ToolChoice struct {
			Type                   string `json:"type"`
			DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if got.ToolChoice.Type != "auto" || !got.ToolChoice.DisableParallelToolUse {
		t.Errorf("tool_choice = %+v, want {type:auto disable_parallel_tool_use:true}", got.ToolChoice)
	}
}

// parallel_tool_calls:false must merge into an explicit tool_choice, not
// clobber it.
func TestTranslateRequest_ParallelToolCallsFalseWithExplicitChoice(t *testing.T) {
	body := []byte(`{"model":"m","parallel_tool_calls":false,"tool_choice":"required",` +
		`"tools":[{"type":"function","function":{"name":"foo"}}],"messages":[{"role":"user","content":"hi"}]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got struct {
		ToolChoice struct {
			Type                   string `json:"type"`
			DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if got.ToolChoice.Type != "any" || !got.ToolChoice.DisableParallelToolUse {
		t.Errorf("tool_choice = %+v, want {type:any disable_parallel_tool_use:true}", got.ToolChoice)
	}
}

func mustCanon(s string) string {
	var v any
	_ = json.Unmarshal([]byte(s), &v)
	b, _ := json.Marshal(v)
	return string(b)
}

// TestTranslateResponse_TextAndTool: text + tool_use → content + tool_calls.
// TestTranslateResponse_Thinking covers the non-streaming response side: a
// thinking block surfaces as reasoning_content/reasoning_signature, not as
// visible content, and finish_reason still reflects the real stop_reason.
func TestTranslateResponse_Thinking(t *testing.T) {
	body := []byte(`{
		"id":"msg_1","type":"message","role":"assistant","model":"claude-x","stop_reason":"end_turn",
		"content":[
			{"type":"thinking","thinking":"reasoning about pelicans","signature":"sig123"},
			{"type":"text","text":"Pouch and Pelé"}
		]
	}`)
	out, err := translateResponse(body, "claude-x")
	if err != nil {
		t.Fatalf("translateResponse error: %v", err)
	}
	var got struct {
		Choices []struct {
			Message struct {
				Content            string `json:"content"`
				ReasoningContent   string `json:"reasoning_content"`
				ReasoningSignature string `json:"reasoning_signature"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid OpenAI response: %v\n%s", err, out)
	}
	msg := got.Choices[0].Message
	if msg.Content != "Pouch and Pelé" {
		t.Errorf("content = %q, thinking text leaked into it?", msg.Content)
	}
	if msg.ReasoningContent != "reasoning about pelicans" || msg.ReasoningSignature != "sig123" {
		t.Errorf("reasoning fields wrong: %+v", msg)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", got.Choices[0].FinishReason)
	}
}

// TestTranslateRequest_ThinkingRoundTrip is the multi-turn regression this
// whole feature exists for: a client echoing back an assistant message that
// carries both reasoning_content/reasoning_signature AND tool_calls (a
// thinking-enabled tool-use turn) must reconstruct the thinking block FIRST
// in the Anthropic content array — Anthropic 400s with "Expected thinking or
// redacted_thinking block, but found tool_use" otherwise.
func TestTranslateRequest_ThinkingRoundTrip(t *testing.T) {
	body := []byte(`{"model":"gpt-x","max_tokens":100,"messages":[
		{"role":"user","content":"weather in SF?"},
		{"role":"assistant","content":null,"reasoning_content":"I should check the weather","reasoning_signature":"sig-abc",
		 "tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]}
	]}`)
	out, err := translateRequest(body)
	if err != nil {
		t.Fatalf("translateRequest error: %v", err)
	}
	var got struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid Anthropic request: %v\n%s", err, out)
	}
	var blocks []struct {
		Type      string `json:"type"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(got.Messages[1].Content, &blocks); err != nil {
		t.Fatalf("assistant content not a block array: %v\n%s", err, out)
	}
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks (thinking, tool_use), got %d: %s", len(blocks), out)
	}
	if blocks[0].Type != "thinking" || blocks[0].Thinking != "I should check the weather" || blocks[0].Signature != "sig-abc" {
		t.Errorf("thinking block wrong or not first: %+v", blocks[0])
	}
	if blocks[1].Type != "tool_use" || blocks[1].Name != "get_weather" {
		t.Errorf("tool_use block wrong: %+v", blocks[1])
	}
}

func TestTranslateResponse_TextAndTool(t *testing.T) {
	body := []byte(`{
		"id":"msg_1","type":"message","role":"assistant","model":"claude-x",
		"content":[
			{"type":"text","text":"let me check"},
			{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":5,"output_tokens":7}
	}`)
	out, err := translateResponse(body, "fallback")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("bad openai body: %v", err)
	}
	msg := got.Choices[0].Message
	if msg.Content == nil || *msg.Content != "let me check" {
		t.Errorf("content = %v", msg.Content)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("want 1 tool_call, got %d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Errorf("tool_call = %+v", tc)
	}
	// arguments is a JSON string.
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments not a JSON string object: %v (%q)", err, tc.Function.Arguments)
	}
	if args["city"] != "SF" {
		t.Errorf("arguments = %v", args)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", got.Choices[0].FinishReason)
	}
}

// TestTranslateResponse_ToolOnly: tool_use with no text → content:null.
func TestTranslateResponse_ToolOnly(t *testing.T) {
	body := []byte(`{
		"id":"msg_2","model":"claude-x",
		"content":[{"type":"tool_use","id":"toolu_9","name":"x","input":{}}],
		"stop_reason":"tool_use"
	}`)
	out, err := translateResponse(body, "fallback")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	// content must be JSON null (present, not "").
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(out, &raw)
	var choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	}
	_ = json.Unmarshal(raw["choices"], &choices)
	if strings.TrimSpace(string(choices[0].Message.Content)) != "null" {
		t.Errorf("content should be null, got %s", choices[0].Message.Content)
	}
	if choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", choices[0].FinishReason)
	}
}

// TestStreaming_ToolCalls feeds an Anthropic tool SSE sequence and checks the
// emitted OpenAI stream reassembles the tool call.
// TestStreaming_Thinking replays real captured extended-thinking SSE events
// (from simonw/llm-anthropic's test_stream_events_thinking.yaml cassette,
// Apache 2.0 — also saved sanitized at
// internal/app/gateway/testdata/fieldmatrix/upstream/messages-anthropic-compat-thinking-stream.sse)
// through the streaming handler: thinking_delta -> reasoning_content deltas,
// signature_delta -> a reasoning_signature delta, and the final text block
// still comes through as ordinary content.
func TestStreaming_Thinking(t *testing.T) {
	h := New().(openaiAnthropic).NewResponseHandler()
	events := []string{
		`data: {"type":"message_start","message":{"id":"msg_01Eg56TYRnKCEgWtZu2yjR1t","model":"claude-haiku-4-5-20251001"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"The user wants"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" two names for a pet pelican."}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EuYDCmMIDBgC-real-signature-blob-truncated-for-test"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"1. Pouch 2. Pel"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`data: {"type":"message_stop"}`,
	}
	var out strings.Builder
	for _, e := range events {
		b, err := h.Feed([]byte(e + "\n\n"))
		if err != nil {
			t.Fatalf("feed: %v", err)
		}
		out.Write(b)
	}
	if _, _, err := h.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	events2 := parseSSEDataLines(t, out.String())
	var reasoning strings.Builder
	var signature string
	var content strings.Builder
	for _, ev := range events2 {
		if rc, ok := ev["reasoning_content"].(string); ok {
			reasoning.WriteString(rc)
		}
		if sig, ok := ev["reasoning_signature"].(string); ok {
			signature = sig
		}
		if c, ok := ev["content"].(string); ok {
			content.WriteString(c)
		}
	}
	if reasoning.String() != "The user wants two names for a pet pelican." {
		t.Errorf("reasoning_content = %q", reasoning.String())
	}
	if signature != "EuYDCmMIDBgC-real-signature-blob-truncated-for-test" {
		t.Errorf("reasoning_signature = %q", signature)
	}
	if content.String() != "1. Pouch 2. Pel" {
		t.Errorf("content = %q", content.String())
	}
}

// parseSSEDataLines extracts every choices[0].delta object from an OpenAI SSE
// stream's data: lines (skipping [DONE]).
func parseSSEDataLines(t *testing.T, sse string) []map[string]any {
	t.Helper()
	var deltas []map[string]any
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev struct {
			Choices []struct {
				Delta map[string]any `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("invalid SSE data JSON %q: %v", payload, err)
		}
		if len(ev.Choices) > 0 {
			deltas = append(deltas, ev.Choices[0].Delta)
		}
	}
	return deltas
}

func TestStreaming_ToolCalls(t *testing.T) {
	h := New().(openaiAnthropic).NewResponseHandler()
	events := []string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_a","name":"get_weather","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"ci"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"ty\":\"SF\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`data: {"type":"message_stop"}`,
	}
	var out strings.Builder
	for _, e := range events {
		b, err := h.Feed([]byte(e + "\n\n"))
		if err != nil {
			t.Fatalf("feed: %v", err)
		}
		out.Write(b)
	}
	if _, _, err := h.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	got := out.String()

	name, id, args, idxSeen, finish, done := scanToolStream(t, got)
	if id != "toolu_a" || name != "get_weather" {
		t.Errorf("header id/name = %q/%q", id, name)
	}
	if idxSeen[0] == false {
		t.Errorf("tool_call index 0 not seen")
	}
	if args != `{"city":"SF"}` {
		t.Errorf("reassembled arguments = %q", args)
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q", finish)
	}
	if !done {
		t.Errorf("missing [DONE]")
	}
}

// TestStreaming_TwoTools: two tool_use blocks get index 0 and 1.
func TestStreaming_TwoTools(t *testing.T) {
	h := New().(openaiAnthropic).NewResponseHandler()
	events := []string{
		`data: {"type":"message_start","message":{"id":"m","model":"c"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t0","name":"a"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"b"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_stop"}`,
	}
	var out strings.Builder
	for _, e := range events {
		b, _ := h.Feed([]byte(e + "\n\n"))
		out.Write(b)
	}
	_, _, _ = h.Flush()
	idxs := collectToolIndexes(t, out.String())
	if !idxs[0] || !idxs[1] {
		t.Errorf("want tool_call indexes 0 and 1, got %v", idxs)
	}
}

// TestStreaming_PlainText regression: text streaming still emits content deltas.
func TestStreaming_PlainText(t *testing.T) {
	h := New().(openaiAnthropic).NewResponseHandler()
	events := []string{
		`data: {"type":"message_start","message":{"id":"m","model":"c"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`data: {"type":"message_stop"}`,
	}
	var out strings.Builder
	for _, e := range events {
		b, _ := h.Feed([]byte(e + "\n\n"))
		out.Write(b)
	}
	_, _, _ = h.Flush()
	got := out.String()
	if !strings.Contains(got, `"content":"hello"`) || !strings.Contains(got, `"content":" world"`) {
		t.Errorf("text deltas missing: %s", got)
	}
	if strings.Contains(got, "tool_calls") {
		t.Errorf("plain text stream must not emit tool_calls: %s", got)
	}
	if !strings.Contains(got, `"finish_reason":"stop"`) || !strings.Contains(got, "[DONE]") {
		t.Errorf("missing finish/done: %s", got)
	}
}

// --- streaming assertion helpers ---

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function *struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

func decodeChunks(t *testing.T, stream string) ([]streamChunk, bool) {
	t.Helper()
	var chunks []streamChunk
	done := false
	for _, line := range strings.Split(stream, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			done = true
			continue
		}
		var c streamChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatalf("bad chunk %q: %v", payload, err)
		}
		chunks = append(chunks, c)
	}
	return chunks, done
}

func scanToolStream(t *testing.T, stream string) (name, id, args string, idxSeen map[int]bool, finish string, done bool) {
	t.Helper()
	idxSeen = map[int]bool{}
	var chunks []streamChunk
	chunks, done = decodeChunks(t, stream)
	for _, c := range chunks {
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				idxSeen[tc.Index] = true
				if tc.Function != nil {
					if tc.Function.Name != "" {
						name = tc.Function.Name
					}
					args += tc.Function.Arguments
				}
				if tc.ID != "" {
					id = tc.ID
				}
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
	}
	return
}

func collectToolIndexes(t *testing.T, stream string) map[int]bool {
	t.Helper()
	seen := map[int]bool{}
	chunks, _ := decodeChunks(t, stream)
	for _, c := range chunks {
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				// only count the header chunk (has id) to check index assignment
				if tc.ID != "" {
					seen[tc.Index] = true
				}
			}
		}
	}
	return seen
}
