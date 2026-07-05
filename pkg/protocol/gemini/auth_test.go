package gemini

import (
	"context"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

func TestStaticAPIKey_AuthHeader(t *testing.T) {
	s := staticAPIKey{key: "my-key"}
	name, value, err := s.AuthHeader(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if name != "x-goog-api-key" {
		t.Errorf("name=%q", name)
	}
	if value != "my-key" {
		t.Errorf("value=%q", value)
	}
}

func TestNewTokenProvider_GeminiKey_Happy(t *testing.T) {
	auth, _ := domain.EncodePayload(domain.AuthTypeGeminiKey, domain.GeminiAuth{APIKey: "ai-key"})
	tp, err := newTokenProvider(context.Background(), auth)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	name, value, err := tp.AuthHeader(context.Background())
	if err != nil {
		t.Fatalf("AuthHeader err=%v", err)
	}
	if name != "x-goog-api-key" || value != "ai-key" {
		t.Errorf("got name=%q value=%q", name, value)
	}
}

func TestNewTokenProvider_GeminiKey_EmptyAPIKey_Error(t *testing.T) {
	auth, _ := domain.EncodePayload(domain.AuthTypeGeminiKey, domain.GeminiAuth{APIKey: ""})
	if _, err := newTokenProvider(context.Background(), auth); err == nil {
		t.Fatal("expected err for empty api key")
	}
}

func TestNewTokenProvider_GeminiKey_BadPayload_Error(t *testing.T) {
	auth := domain.AuthConfig{Type: domain.AuthTypeGeminiKey, Payload: []byte(`not json`)}
	if _, err := newTokenProvider(context.Background(), auth); err == nil {
		t.Fatal("expected unmarshal error")
	}
}

func TestNewTokenProvider_OAuth2SA_EmptySAJSON_Error(t *testing.T) {
	auth, _ := domain.EncodePayload(domain.AuthTypeOAuth2SA, domain.OAuth2SAAuth{ServiceAccountJSON: ""})
	if _, err := newTokenProvider(context.Background(), auth); err == nil {
		t.Fatal("expected err for empty SA JSON")
	}
}

func TestNewTokenProvider_OAuth2SA_BadPayload_Error(t *testing.T) {
	auth := domain.AuthConfig{Type: domain.AuthTypeOAuth2SA, Payload: []byte(`bad`)}
	if _, err := newTokenProvider(context.Background(), auth); err == nil {
		t.Fatal("expected unmarshal error")
	}
}

func TestNewTokenProvider_UnsupportedType_Error(t *testing.T) {
	auth := domain.AuthConfig{Type: "bearer"} // "bearer" isn't in gemini's supported list
	if _, err := newTokenProvider(context.Background(), auth); err == nil {
		t.Fatal("expected err for unsupported type")
	}
}
