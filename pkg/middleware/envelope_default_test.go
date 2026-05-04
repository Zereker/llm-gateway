package middleware

import (
	"strings"
	"testing"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

func TestDefaultParser_OpenAIHappy(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	p := DefaultParser{}
	req, err := p.Parse(body, domain.ProtoOpenAI, domain.ModalityChat)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if req.Model != "gpt-4o" {
		t.Errorf("Model = %q, want gpt-4o", req.Model)
	}
}

func TestDefaultParser_RejectsEmpty(t *testing.T) {
	p := DefaultParser{}
	if _, err := p.Parse(nil, domain.ProtoOpenAI, domain.ModalityChat); err == nil {
		t.Fatal("want error for empty body")
	}
}

func TestDefaultParser_RejectsMissingModel(t *testing.T) {
	p := DefaultParser{}
	body := []byte(`{"stream":false}`)
	_, err := p.Parse(body, domain.ProtoOpenAI, domain.ModalityChat)
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Errorf("want error mentioning 'model', got %v", err)
	}
}

func TestDefaultParser_AnthropicAccepted(t *testing.T) {
	p := DefaultParser{}
	body := []byte(`{"model":"claude-3.5-sonnet","max_tokens":1024,"messages":[]}`)
	got, err := p.Parse(body, domain.ProtoAnthropic, domain.ModalityChat)
	if err != nil {
		t.Fatalf("anthropic body should parse: %v", err)
	}
	if got.Model != "claude-3.5-sonnet" {
		t.Errorf("model = %q", got.Model)
	}
}

// Gemini 不暴露成客户端入口（网关只对外开 chat/messages/responses 三种协议），
// DefaultParser 收到 ProtoGemini 应该报错——这种 case 只可能是 router 配置错。
func TestDefaultParser_RejectsGemini(t *testing.T) {
	p := DefaultParser{}
	body := []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`)
	_, err := p.Parse(body, domain.ProtoGemini, domain.ModalityChat)
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol") {
		t.Errorf("want unsupported protocol error, got %v", err)
	}
}
