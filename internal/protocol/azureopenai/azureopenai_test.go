package azureopenai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

func bearerAuth(key string) domain.AuthConfig {
	return domain.AuthConfig{Type: domain.AuthTypeBearer, Payload: json.RawMessage(`{"api_key":"` + key + `"}`)}
}

func buildReq(t *testing.T, ep *domain.Endpoint) *http.Request {
	t.Helper()
	sess, err := Factory{}.NewSession(context.Background(), ep, &domain.RequestEnvelope{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	req, err := sess.BuildRequest([]byte(`{"model":"x"}`), http.Header{})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	return req
}

func TestAzure_AuthAndAPIVersion(t *testing.T) {
	ep := &domain.Endpoint{
		Vendor: "azure-openai", Protocol: domain.ProtoOpenAI,
		Auth:    bearerAuth("azkey123"),
		Routing: domain.RoutingConfig{URL: "https://my.openai.azure.com/openai/deployments/gpt4o/chat/completions", APIVersion: "2024-06-01"},
	}
	req := buildReq(t, ep)

	// api-key header (not Authorization: Bearer)
	if got := req.Header.Get("api-key"); got != "azkey123" {
		t.Errorf("api-key = %q, want azkey123", got)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("Azure should not send an Authorization header")
	}
	// api-version gets appended to the query
	if !strings.Contains(req.URL.RawQuery, "api-version=2024-06-01") {
		t.Errorf("URL query = %q, want api-version=2024-06-01", req.URL.RawQuery)
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Error("Content-Type should be application/json")
	}
}

func TestAzure_ExistingAPIVersionKept(t *testing.T) {
	ep := &domain.Endpoint{
		Auth:    bearerAuth("k"),
		Routing: domain.RoutingConfig{URL: "https://x.openai.azure.com/a/b?api-version=2023-01-01", APIVersion: "2024-06-01"},
	}
	req := buildReq(t, ep)
	if !strings.Contains(req.URL.RawQuery, "api-version=2023-01-01") || strings.Contains(req.URL.RawQuery, "2024-06-01") {
		t.Errorf("existing api-version should be kept, not overwritten, got %q", req.URL.RawQuery)
	}
}

func TestAzure_WrongAuthType(t *testing.T) {
	ep := &domain.Endpoint{
		Auth:    domain.AuthConfig{Type: domain.AuthTypeXAPIKey, Payload: json.RawMessage(`{"api_key":"k"}`)},
		Routing: domain.RoutingConfig{URL: "https://x/y"},
	}
	sess, _ := Factory{}.NewSession(context.Background(), ep, &domain.RequestEnvelope{})
	if _, err := sess.BuildRequest([]byte(`{}`), http.Header{}); err == nil {
		t.Error("non-bearer auth should return an error")
	}
}

// Factory registration + inherited openai Classify (Azure error JSON is OpenAI-shaped).
func TestAzure_FactoryMetadataWithClassify(t *testing.T) {
	f := Factory{}
	if f.Metadata().Vendor != "azure-openai" {
		t.Errorf("vendor = %q", f.Metadata().Vendor)
	}
	// inherited Classify is usable (embeds openai.Factory)
	var _ protocol.Classifier = f
}
