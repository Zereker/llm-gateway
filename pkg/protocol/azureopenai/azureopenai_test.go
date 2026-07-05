package azureopenai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
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

	// api-key 头（不是 Authorization: Bearer）
	if got := req.Header.Get("api-key"); got != "azkey123" {
		t.Errorf("api-key = %q, want azkey123", got)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("Azure 不应发 Authorization 头")
	}
	// api-version 被补进 query
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
		t.Errorf("既有 api-version 应保留不被覆盖，got %q", req.URL.RawQuery)
	}
}

func TestAzure_WrongAuthType(t *testing.T) {
	ep := &domain.Endpoint{
		Auth:    domain.AuthConfig{Type: domain.AuthTypeXAPIKey, Payload: json.RawMessage(`{"api_key":"k"}`)},
		Routing: domain.RoutingConfig{URL: "https://x/y"},
	}
	sess, _ := Factory{}.NewSession(context.Background(), ep, &domain.RequestEnvelope{})
	if _, err := sess.BuildRequest([]byte(`{}`), http.Header{}); err == nil {
		t.Error("非 bearer auth 应报错")
	}
}

// Factory 注册 + 继承 openai 的 Classify（Azure 错误 JSON 是 OpenAI 形状）。
func TestAzure_FactoryRegisteredWithClassify(t *testing.T) {
	if protocol.LookupFactory("azure-openai") == nil {
		t.Fatal("azure-openai vendor 未注册")
	}
	f := Factory{}
	if f.Metadata().Vendor != "azure-openai" {
		t.Errorf("vendor = %q", f.Metadata().Vendor)
	}
	// 继承的 Classify 可用（embed openai.Factory）
	var _ protocol.Classifier = f
}
