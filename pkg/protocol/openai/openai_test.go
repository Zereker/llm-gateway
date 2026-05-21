package openai

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/zereker/llm-gateway/pkg/adapter"
	"github.com/zereker/llm-gateway/pkg/domain"
)

// **v0.5 slim 后**：openai adapter 只剩 BuildRequest + Close。
// SSE 解析 / usage 提取等逻辑搬到 pkg/translator/identity，对应测试在那个包测。
//
// 这里保留：Factory 注册 / BuildRequest URL+auth+body 透传 / 错误路径。

// bearerEP 构造一个带 Bearer 鉴权的 OpenAI Endpoint。
func bearerEP(url, key string) *domain.Endpoint {
	auth, err := domain.EncodePayload(domain.AuthTypeBearer, domain.BearerAuth{APIKey: key})
	if err != nil {
		panic(err)
	}
	return &domain.Endpoint{
		Vendor:  "openai",
		Auth:    auth,
		Routing: domain.RoutingConfig{URL: url},
	}
}

func TestFactory_Metadata(t *testing.T) {
	m := Factory{}.Metadata()
	if m.Vendor != "openai" {
		t.Errorf("Vendor = %q, want openai", m.Vendor)
	}
}

func TestAdapter_Registered(t *testing.T) {
	// vendor 适配器注册（Handler 由 protocol.DefaultLookup 在请求时动态组合）
	if f := adapter.Get("openai"); f == nil {
		t.Fatal("openai adapter not registered")
	}
	// alias 同样注册
	if f := adapter.Get("ark"); f == nil {
		t.Fatal("ark alias adapter not registered")
	}
}

func TestSession_BuildRequest(t *testing.T) {
	ep := bearerEP("https://api.openai.com/v1/chat/completions", "sk-test")
	body := []byte(`{"model":"gpt-4o","stream":false,"messages":[]}`)
	s := newSession(context.Background(), ep, &domain.RequestEnvelope{})

	req, err := s.BuildRequest(body)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.URL.String() != ep.Routing.URL {
		t.Errorf("URL = %s", req.URL.String())
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q", got)
	}
	gotBody, _ := io.ReadAll(req.Body)
	if !bytes.Equal(gotBody, body) {
		t.Errorf("body = %s, want %s (translator handles content; adapter just transports)", gotBody, body)
	}
}

func TestSession_NoAPIKeyOmitsHeader(t *testing.T) {
	ep := bearerEP("u", "")
	s := newSession(context.Background(), ep, &domain.RequestEnvelope{})
	req, err := s.BuildRequest([]byte(`{}`))
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization should be empty: %q", got)
	}
}

func TestSession_RejectsNonBearerAuth(t *testing.T) {
	auth, _ := domain.EncodePayload(domain.AuthTypeXAPIKey, domain.XAPIKeyAuth{APIKey: "k"})
	ep := &domain.Endpoint{
		Vendor:  "anthropic",
		Auth:    auth,
		Routing: domain.RoutingConfig{URL: "u"},
	}
	s := newSession(context.Background(), ep, &domain.RequestEnvelope{})
	if _, err := s.BuildRequest([]byte(`{}`)); err == nil {
		t.Error("expected error for non-bearer auth")
	}
}

func TestSession_RejectsEmptyURL(t *testing.T) {
	ep := bearerEP("", "k")
	s := newSession(context.Background(), ep, &domain.RequestEnvelope{})
	if _, err := s.BuildRequest([]byte(`{}`)); err == nil {
		t.Error("expected error for empty URL")
	}
}

func TestSession_CloseIdempotent(t *testing.T) {
	s := newSession(context.Background(), bearerEP("u", "k"), &domain.RequestEnvelope{})
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
