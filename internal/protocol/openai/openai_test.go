package openai

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/zereker/llm-gateway/internal/domain"
)

// **After the v0.5 slim-down**: the openai adapter only has BuildRequest +
// Close left. SSE parsing / usage extraction etc. moved to
// internal/translator/identity, with corresponding tests in that package.
//
// What's kept here: Factory registration / BuildRequest URL+auth+body
// passthrough / error paths.

// bearerEP builds an OpenAI Endpoint with Bearer auth.
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

func TestSession_BuildRequest(t *testing.T) {
	ep := bearerEP("https://api.openai.com/v1/chat/completions", "sk-test")
	body := []byte(`{"model":"gpt-4o","stream":false,"messages":[]}`)
	s := newSession(context.Background(), ep, &domain.RequestEnvelope{})

	req, err := s.BuildRequest(body, nil)
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
	req, err := s.BuildRequest([]byte(`{}`), nil)
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
	if _, err := s.BuildRequest([]byte(`{}`), nil); err == nil {
		t.Error("expected error for non-bearer auth")
	}
}

func TestSession_RejectsEmptyURL(t *testing.T) {
	ep := bearerEP("", "k")
	s := newSession(context.Background(), ep, &domain.RequestEnvelope{})
	if _, err := s.BuildRequest([]byte(`{}`), nil); err == nil {
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

// TestSession_ExtraHeaders verifies:
//   - extraHeaders are copied into req.Header
//   - protocol-required headers (Content-Type / Authorization) are written
//     last by the adapter, overriding quirks
func TestSession_ExtraHeaders(t *testing.T) {
	s := newSession(context.Background(), bearerEP("https://api.example.com/v1/chat/completions", "real-key"), &domain.RequestEnvelope{})

	extra := http.Header{}
	extra.Set("X-Custom-Tag", "prod")
	// deliberately have quirks write an Authorization — the adapter must override it back to real-key
	extra.Set("Authorization", "Bearer attacker-key")

	req, err := s.BuildRequest([]byte(`{}`), extra)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}

	if got := req.Header.Get("X-Custom-Tag"); got != "prod" {
		t.Errorf("X-Custom-Tag=%q, want \"prod\"", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer real-key" {
		t.Errorf("Authorization=%q, want adapter to override it back to real-key", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type=%q, want adapter to force application/json", got)
	}
}
