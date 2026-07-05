package cohere

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

func TestCohere_BearerAuth(t *testing.T) {
	ep := &domain.Endpoint{
		Vendor: "cohere", Protocol: domain.ProtoCohere,
		Auth:    domain.AuthConfig{Type: domain.AuthTypeBearer, Payload: json.RawMessage(`{"api_key":"co-key"}`)},
		Routing: domain.RoutingConfig{URL: "https://api.cohere.com/v2/chat"},
	}
	sess, err := Factory{}.NewSession(context.Background(), ep, &domain.RequestEnvelope{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	req, err := sess.BuildRequest([]byte(`{"model":"command-r","messages":[]}`), http.Header{})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.Header.Get("Authorization") != "Bearer co-key" {
		t.Errorf("Authorization = %q", req.Header.Get("Authorization"))
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Error("Content-Type should be application/json")
	}
	if req.URL.String() != "https://api.cohere.com/v2/chat" {
		t.Errorf("URL = %q", req.URL.String())
	}
}

func TestCohere_WrongAuthType(t *testing.T) {
	ep := &domain.Endpoint{
		Auth:    domain.AuthConfig{Type: domain.AuthTypeXAPIKey, Payload: json.RawMessage(`{"api_key":"k"}`)},
		Routing: domain.RoutingConfig{URL: "https://x/v2/chat"},
	}
	sess, _ := Factory{}.NewSession(context.Background(), ep, &domain.RequestEnvelope{})
	if _, err := sess.BuildRequest([]byte(`{}`), http.Header{}); err == nil {
		t.Error("non-bearer auth should error")
	}
}

func TestCohere_FactoryRegistered(t *testing.T) {
	if protocol.LookupFactory("cohere") == nil {
		t.Fatal("cohere vendor not registered")
	}
	f := Factory{}
	if f.Metadata().Vendor != "cohere" {
		t.Error("wrong vendor name")
	}
}
