package middleware

import (
	"strings"
	"testing"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

func TestDefaultDetector(t *testing.T) {
	cases := []struct {
		path    string
		proto   domain.Protocol
		modal   domain.Modality
	}{
		{"/v1/chat/completions", domain.ProtoOpenAI, domain.ModalityChat},
		{"/openai/v1/chat/completions?api-version=2024", domain.ProtoOpenAI, domain.ModalityChat},
		{"/v1/messages", domain.ProtoAnthropic, domain.ModalityChat},
		{"/v1/embeddings", domain.ProtoOpenAI, domain.ModalityEmbedding},
		{"/v1/images/generations", domain.ProtoOpenAI, domain.ModalityImage},
		{"/v1beta/models/gemini-2.0-pro:generateContent", domain.ProtoGemini, domain.ModalityChat},
		{"/healthz", domain.ProtoUnknown, domain.ModalityChat},
	}
	d := DefaultDetector{}
	for _, c := range cases {
		gotP, gotM := d.Detect(c.path, nil)
		if gotP != c.proto {
			t.Errorf("%s: proto = %v, want %v", c.path, gotP, c.proto)
		}
		if gotM != c.modal {
			t.Errorf("%s: modal = %v, want %v", c.path, gotM, c.modal)
		}
	}
}

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
	if !req.Stream {
		t.Error("Stream should be true")
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

func TestDefaultParser_RejectsNonOpenAIProto(t *testing.T) {
	p := DefaultParser{}
	body := []byte(`{"model":"claude-3.5-sonnet"}`)
	_, err := p.Parse(body, domain.ProtoAnthropic, domain.ModalityChat)
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("want unsupported error, got %v", err)
	}
}
