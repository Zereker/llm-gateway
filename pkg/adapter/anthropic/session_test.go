package anthropic

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/zereker/llm-gateway/pkg/adapter"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/repo"
)

func mkXAPIKeyEP(url, key string) *domain.Endpoint {
	auth, err := repo.EncodePayload(repo.AuthTypeXAPIKey, repo.XAPIKeyAuth{APIKey: key})
	if err != nil {
		panic(err)
	}
	return &domain.Endpoint{
		Vendor:  "anthropic",
		Auth:    auth,
		Routing: repo.RoutingConfig{URL: url},
	}
}

func TestFactory_Metadata(t *testing.T) {
	m := Factory{}.Metadata()
	if m.Vendor != "anthropic" {
		t.Errorf("Vendor=%q", m.Vendor)
	}
	if m.NativeProtocol != domain.ProtoAnthropic {
		t.Errorf("NativeProtocol=%v", m.NativeProtocol)
	}
}

func TestFactory_RegisteredInRegistry(t *testing.T) {
	if f := adapter.Get("anthropic"); f == nil {
		t.Fatal("anthropic factory not registered")
	}
}

func TestSession_BuildRequest_SetsXAPIKeyAndVersion(t *testing.T) {
	ep := mkXAPIKeyEP("https://api.anthropic.com/v1/messages", "sk-ant-test")
	s := newSession(context.Background(), ep)

	body := []byte(`{"model":"claude-3","messages":[]}`)
	req, err := s.BuildRequest(body)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.URL.String() != ep.Routing.URL {
		t.Errorf("URL=%q", req.URL.String())
	}
	if req.Header.Get("x-api-key") != "sk-ant-test" {
		t.Errorf("x-api-key=%q", req.Header.Get("x-api-key"))
	}
	if req.Header.Get("anthropic-version") != anthropicAPIVersion {
		t.Errorf("anthropic-version=%q", req.Header.Get("anthropic-version"))
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type=%q", req.Header.Get("Content-Type"))
	}
	gotBody, _ := io.ReadAll(req.Body)
	if !bytes.Equal(gotBody, body) {
		t.Errorf("body=%s, want=%s", gotBody, body)
	}
}

func TestSession_BuildRequest_EmptyURL_Error(t *testing.T) {
	ep := mkXAPIKeyEP("", "k")
	s := newSession(context.Background(), ep)
	if _, err := s.BuildRequest([]byte(`{}`)); err == nil {
		t.Fatal("expected err for empty URL")
	}
}

func TestSession_BuildRequest_WrongAuthType_Error(t *testing.T) {
	auth, _ := repo.EncodePayload(repo.AuthTypeBearer, repo.BearerAuth{APIKey: "k"})
	ep := &domain.Endpoint{Auth: auth, Routing: repo.RoutingConfig{URL: "u"}}
	s := newSession(context.Background(), ep)
	if _, err := s.BuildRequest([]byte(`{}`)); err == nil {
		t.Fatal("expected err for wrong auth type")
	}
}

func TestSession_BuildRequest_EmptyAPIKey_Error(t *testing.T) {
	ep := mkXAPIKeyEP("u", "")
	s := newSession(context.Background(), ep)
	if _, err := s.BuildRequest([]byte(`{}`)); err == nil {
		t.Fatal("expected err for empty API key")
	}
}

func TestSession_BuildRequest_BadPayload_Error(t *testing.T) {
	// 故意构造一个 payload 不是 XAPIKeyAuth JSON
	ep := &domain.Endpoint{
		Auth: repo.AuthConfig{
			Type:    repo.AuthTypeXAPIKey,
			Payload: []byte(`not json`),
		},
		Routing: repo.RoutingConfig{URL: "u"},
	}
	s := newSession(context.Background(), ep)
	if _, err := s.BuildRequest([]byte(`{}`)); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestSession_CloseIdempotent(t *testing.T) {
	s := newSession(context.Background(), mkXAPIKeyEP("u", "k"))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !s.closed {
		t.Error("closed flag not set")
	}
}
