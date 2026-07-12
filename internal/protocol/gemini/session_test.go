package gemini

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/zereker/llm-gateway/internal/domain"
)

// fakeTokenProvider for testing the session BuildRequest with arbitrary header.
type fakeTokenProvider struct {
	hdrName  string
	hdrValue string
	err      error
}

func (f fakeTokenProvider) AuthHeader(_ context.Context) (string, string, error) {
	return f.hdrName, f.hdrValue, f.err
}

func TestFactory_Metadata(t *testing.T) {
	m := Factory{}.Metadata()
	if m.Vendor != "gemini" {
		t.Errorf("Vendor=%q", m.Vendor)
	}
}

func TestSession_BuildRequest_StaticAPIKey(t *testing.T) {
	ep := &domain.Endpoint{
		Routing: domain.RoutingConfig{URL: "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent"},
	}
	tp := fakeTokenProvider{hdrName: "x-goog-api-key", hdrValue: "ai-studio-key"}
	s := newSession(context.Background(), ep, tp, false)

	body := []byte(`{"contents":[]}`)
	req, err := s.BuildRequest(body, nil)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.URL.String() != ep.Routing.URL {
		t.Errorf("URL=%q", req.URL.String())
	}
	if req.Header.Get("x-goog-api-key") != "ai-studio-key" {
		t.Errorf("x-goog-api-key=%q", req.Header.Get("x-goog-api-key"))
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type=%q", req.Header.Get("Content-Type"))
	}
	got, _ := io.ReadAll(req.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("body=%s", got)
	}
}

func TestSession_BuildRequest_OAuthBearer(t *testing.T) {
	ep := &domain.Endpoint{Routing: domain.RoutingConfig{URL: "https://x"}}
	tp := fakeTokenProvider{hdrName: "Authorization", hdrValue: "Bearer ya29.xxxx"}
	s := newSession(context.Background(), ep, tp, false)

	req, err := s.BuildRequest([]byte(`{}`), nil)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.Header.Get("Authorization") != "Bearer ya29.xxxx" {
		t.Errorf("Authorization=%q", req.Header.Get("Authorization"))
	}
}

func TestSession_BuildRequest_EmptyURL_Error(t *testing.T) {
	ep := &domain.Endpoint{Routing: domain.RoutingConfig{URL: ""}}
	s := newSession(context.Background(), ep, fakeTokenProvider{hdrName: "x", hdrValue: "y"}, false)
	if _, err := s.BuildRequest([]byte(`{}`), nil); err == nil {
		t.Fatal("expected err for empty URL")
	}
}

func TestSession_BuildRequest_TokenProviderErr(t *testing.T) {
	ep := &domain.Endpoint{Routing: domain.RoutingConfig{URL: "u"}}
	tp := fakeTokenProvider{err: errors.New("oauth failed")}
	s := newSession(context.Background(), ep, tp, false)
	if _, err := s.BuildRequest([]byte(`{}`), nil); err == nil {
		t.Fatal("expected err from token provider")
	}
}

func TestSession_CloseIdempotent(t *testing.T) {
	s := newSession(context.Background(), &domain.Endpoint{Routing: domain.RoutingConfig{URL: "u"}}, fakeTokenProvider{}, false)
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
