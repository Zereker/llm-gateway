package gemini

import (
	"context"
	"testing"

	"github.com/zereker/llm-gateway/pkg/repo"
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
	auth, _ := repo.EncodePayload(repo.AuthTypeGeminiKey, repo.GeminiAuth{APIKey: "ai-key"})
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
	auth, _ := repo.EncodePayload(repo.AuthTypeGeminiKey, repo.GeminiAuth{APIKey: ""})
	if _, err := newTokenProvider(context.Background(), auth); err == nil {
		t.Fatal("expected err for empty api key")
	}
}

func TestNewTokenProvider_GeminiKey_BadPayload_Error(t *testing.T) {
	auth := repo.AuthConfig{Type: repo.AuthTypeGeminiKey, Payload: []byte(`not json`)}
	if _, err := newTokenProvider(context.Background(), auth); err == nil {
		t.Fatal("expected unmarshal error")
	}
}

func TestNewTokenProvider_OAuth2SA_EmptySAJSON_Error(t *testing.T) {
	auth, _ := repo.EncodePayload(repo.AuthTypeOAuth2SA, repo.OAuth2SAAuth{ServiceAccountJSON: ""})
	if _, err := newTokenProvider(context.Background(), auth); err == nil {
		t.Fatal("expected err for empty SA JSON")
	}
}

func TestNewTokenProvider_OAuth2SA_BadPayload_Error(t *testing.T) {
	auth := repo.AuthConfig{Type: repo.AuthTypeOAuth2SA, Payload: []byte(`bad`)}
	if _, err := newTokenProvider(context.Background(), auth); err == nil {
		t.Fatal("expected unmarshal error")
	}
}

func TestNewTokenProvider_UnsupportedType_Error(t *testing.T) {
	auth := repo.AuthConfig{Type: "bearer"} // bearer 不在 gemini 支持列表里
	if _, err := newTokenProvider(context.Background(), auth); err == nil {
		t.Fatal("expected err for unsupported type")
	}
}

func TestRandID_Format(t *testing.T) {
	got := randID()
	if len(got) != 24 { // 12 bytes hex = 24 chars
		t.Errorf("len=%d, want=24", len(got))
	}
}
