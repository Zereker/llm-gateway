package openai_anthropic

import (
	"encoding/json"
	"strings"
	"testing"
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
		{"none", `"none"`, ``},
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

func mustCanon(s string) string {
	var v any
	_ = json.Unmarshal([]byte(s), &v)
	b, _ := json.Marshal(v)
	return string(b)
}

// TestTranslateResponse_TextAndTool: text + tool_use → content + tool_calls.
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
