package bedrock

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

func sigv4Auth(access, secret, region string) domain.AuthConfig {
	return domain.AuthConfig{
		Type:    domain.AuthTypeAWSSigV4,
		Payload: json.RawMessage(`{"access_key":"` + access + `","secret_key":"` + secret + `","region":"` + region + `"}`),
	}
}

func TestToBedrockBody(t *testing.T) {
	in := `{"model":"anthropic.claude","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	out, err := toBedrockBody([]byte(in))
	if err != nil {
		t.Fatalf("toBedrockBody: %v", err)
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(out, &m)
	if _, ok := m["model"]; ok {
		t.Error("model 应被删除（modelId 在 URL）")
	}
	if _, ok := m["stream"]; ok {
		t.Error("stream 应被删除（v1 非流式）")
	}
	if string(m["anthropic_version"]) != `"bedrock-2023-05-31"` {
		t.Errorf("anthropic_version = %s, want bedrock-2023-05-31", m["anthropic_version"])
	}
	if string(m["max_tokens"]) != "100" {
		t.Errorf("max_tokens 应保留原值, got %s", m["max_tokens"])
	}
	if !strings.Contains(string(out), `"messages"`) {
		t.Error("messages 应保留")
	}
}

func TestBedrock_SignedRequest(t *testing.T) {
	ep := &domain.Endpoint{
		Vendor: "bedrock", Protocol: domain.ProtoAnthropic,
		Auth:    sigv4Auth("AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1"),
		Routing: domain.RoutingConfig{URL: "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20240620-v1:0/invoke"},
	}
	sess, err := Factory{}.NewSession(context.Background(), ep, &domain.RequestEnvelope{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	req, err := sess.BuildRequest([]byte(`{"model":"x","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`), http.Header{})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}

	authz := req.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization 不是 SigV4: %q", authz)
	}
	// scope 正确：region/service/aws4_request
	if !strings.Contains(authz, "us-east-1/bedrock/aws4_request") {
		t.Errorf("SigV4 scope 错: %q", authz)
	}
	if !strings.Contains(authz, "Credential=AKIDEXAMPLE/") {
		t.Errorf("Credential 错: %q", authz)
	}
	// SignedHeaders 至少含 host
	if !strings.Contains(authz, "SignedHeaders=") || !strings.Contains(authz, "host") {
		t.Errorf("SignedHeaders 缺 host: %q", authz)
	}
	if req.Header.Get("X-Amz-Date") == "" {
		t.Error("缺 X-Amz-Date")
	}

	// body 已改写
	b, _ := io.ReadAll(req.Body)
	if strings.Contains(string(b), `"model"`) || !strings.Contains(string(b), "bedrock-2023-05-31") {
		t.Errorf("body 未正确改写: %s", b)
	}
}

func TestBedrock_WrongAuthAndRegion(t *testing.T) {
	// 非 sigv4 auth
	ep := &domain.Endpoint{
		Auth:    domain.AuthConfig{Type: domain.AuthTypeBearer, Payload: json.RawMessage(`{"api_key":"k"}`)},
		Routing: domain.RoutingConfig{URL: "https://x/invoke"},
	}
	sess, _ := Factory{}.NewSession(context.Background(), ep, &domain.RequestEnvelope{})
	if _, err := sess.BuildRequest([]byte(`{}`), http.Header{}); err == nil {
		t.Error("非 aws-sigv4 auth 应报错")
	}
	// 缺 region
	ep2 := &domain.Endpoint{
		Auth:    sigv4Auth("AK", "SK", ""),
		Routing: domain.RoutingConfig{URL: "https://x/invoke"},
	}
	sess2, _ := Factory{}.NewSession(context.Background(), ep2, &domain.RequestEnvelope{})
	if _, err := sess2.BuildRequest([]byte(`{}`), http.Header{}); err == nil {
		t.Error("缺 region 应报错")
	}
}

func TestBedrock_FactoryRegistered(t *testing.T) {
	if protocol.LookupFactory("bedrock") == nil {
		t.Fatal("bedrock vendor 未注册")
	}
	f := Factory{}
	if f.Metadata().Vendor != "bedrock" {
		t.Error("vendor 名错")
	}
}
