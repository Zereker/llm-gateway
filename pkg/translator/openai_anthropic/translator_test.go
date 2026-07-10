package openai_anthropic

import (
	"encoding/json"
	"testing"
)

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
