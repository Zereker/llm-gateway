package responses_openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/internal/usage/extractor"
)

func TestTranslateRequest_StringInputWithInstructions(t *testing.T) {
	src := []byte(`{"model":"gpt-4o","input":"hello","instructions":"You are helpful.","stream":false}`)
	got, err := translateRequest(src)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	var out chatRequest
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Model != "gpt-4o" {
		t.Errorf("model=%q", out.Model)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages=%d want=2", len(out.Messages))
	}
	if out.Messages[0].Role != "system" || out.Messages[0].Content != "You are helpful." {
		t.Errorf("system msg wrong: %+v", out.Messages[0])
	}
	if out.Messages[1].Role != "user" || out.Messages[1].Content != "hello" {
		t.Errorf("user msg wrong: %+v", out.Messages[1])
	}
}

func TestTranslateRequest_NoInstructions(t *testing.T) {
	src := []byte(`{"model":"gpt-4o","input":"ping"}`)
	got, err := translateRequest(src)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	var out chatRequest
	_ = json.Unmarshal(got, &out)
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
		t.Errorf("expected single user message: %+v", out.Messages)
	}
}

func TestTranslateRequest_MessageArrayInput(t *testing.T) {
	src := []byte(`{"model":"gpt-4o","input":[{"role":"user","content":"hi"}]}`)
	got, err := translateRequest(src)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	var out chatRequest
	_ = json.Unmarshal(got, &out)
	if len(out.Messages) != 1 || out.Messages[0].Content != "hi" {
		t.Errorf("messages=%+v", out.Messages)
	}
}

func TestTranslateRequest_MissingModel_Error(t *testing.T) {
	src := []byte(`{"input":"hi"}`)
	if _, err := translateRequest(src); err == nil {
		t.Fatal("expected error for missing model")
	}
}

func TestResponseHandler_TranslatesChatToResponses(t *testing.T) {
	chat := `{"id":"chatcmpl-abc","object":"chat.completion","created":1700000000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hello!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`
	h := &responseHandler{ex: extractor.NewOpenAI()}
	_, _ = h.Feed([]byte(chat))
	body, usage, _ := h.Flush()

	s := string(body)
	if !strings.Contains(s, `"object":"response"`) {
		t.Errorf("missing object:response, got %s", s)
	}
	if !strings.Contains(s, `"output_text"`) {
		t.Errorf("missing output_text, got %s", s)
	}
	if !strings.Contains(s, `"input_tokens":10`) || !strings.Contains(s, `"output_tokens":3`) {
		t.Errorf("missing token counts, got %s", s)
	}
	if !strings.Contains(s, `"text":"hello!"`) {
		t.Errorf("missing assistant content, got %s", s)
	}
	// usage should be obtained from the OpenAI extractor (side-channel extraction)
	if usage == nil {
		t.Error("usage should be extracted")
	} else if usage.Total != 13 {
		t.Errorf("usage.Total=%d want 13", usage.Total)
	}
}
