package anthropic_openai

import (
	"encoding/json"
	"strings"
	"testing"
)

// A standard Anthropic SDK request sends content (and system) as an array of
// content blocks. translateRequest must accept it, not reject it with a parse
// error.
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
