package gemini

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// tokenProvider Gemini 的认证抽象——三种凭证形态：
//
//	gemini-key   → AI Studio API key（x-goog-api-key 头）
//	vertex-adc   → Application Default Credentials（Authorization: Bearer <oauth2_token>）
//	oauth2-sa    → 嵌入的 Service Account JSON（Authorization: Bearer <oauth2_token>）
//
// session 内一次性 build；token 缓存复用 oauth2.TokenSource 内置（自动刷新）。
type tokenProvider interface {
	// AuthHeader 返回要加到 HTTP request 的 (header_name, header_value)。
	// 失败（OAuth refresh 错等）返回 err。
	AuthHeader(ctx context.Context) (string, string, error)
}

// newTokenProvider 按 auth.type 构造合适的 provider。
func newTokenProvider(ctx context.Context, auth domain.AuthConfig) (tokenProvider, error) {
	switch auth.Type {
	case domain.AuthTypeGeminiKey:
		var p domain.GeminiAuth
		if err := json.Unmarshal(auth.Payload, &p); err != nil {
			return nil, fmt.Errorf("gemini-key payload: %w", err)
		}
		if p.APIKey == "" {
			return nil, fmt.Errorf("gemini-key: api_key empty")
		}
		return staticAPIKey{key: p.APIKey}, nil

	case domain.AuthTypeVertexADC:
		// payload 可选 scopes；不填走 default
		var p domain.VertexADCAuth
		if len(auth.Payload) > 0 {
			_ = json.Unmarshal(auth.Payload, &p) // 失败忽略，用 default scopes
		}
		scopes := p.Scopes
		if len(scopes) == 0 {
			scopes = []string{"https://www.googleapis.com/auth/cloud-platform"}
		}
		creds, err := google.FindDefaultCredentials(ctx, scopes...)
		if err != nil {
			return nil, fmt.Errorf("vertex-adc: find default credentials: %w", err)
		}
		return &oauthBearer{ts: creds.TokenSource}, nil

	case domain.AuthTypeOAuth2SA:
		var p domain.OAuth2SAAuth
		if err := json.Unmarshal(auth.Payload, &p); err != nil {
			return nil, fmt.Errorf("oauth2-sa payload: %w", err)
		}
		if p.ServiceAccountJSON == "" {
			return nil, fmt.Errorf("oauth2-sa: service_account_json empty")
		}
		creds, err := google.CredentialsFromJSON(ctx,
			[]byte(p.ServiceAccountJSON),
			"https://www.googleapis.com/auth/cloud-platform",
		)
		if err != nil {
			return nil, fmt.Errorf("oauth2-sa: parse SA JSON: %w", err)
		}
		return &oauthBearer{ts: creds.TokenSource}, nil

	default:
		return nil, fmt.Errorf("gemini adapter: unsupported auth type %q (want %s|%s|%s)",
			auth.Type, domain.AuthTypeGeminiKey, domain.AuthTypeVertexADC, domain.AuthTypeOAuth2SA)
	}
}

// staticAPIKey AI Studio 用 x-goog-api-key 头。
type staticAPIKey struct {
	key string
}

func (s staticAPIKey) AuthHeader(_ context.Context) (string, string, error) {
	return "x-goog-api-key", s.key, nil
}

// oauthBearer Vertex 走 Authorization: Bearer <oauth2 access token>。
//
// oauth2.TokenSource 内置缓存（go-oauth2 lib），同一 source 重复 .Token() 调用
// 大多数返回缓存 token，只在过期前自动 refresh。
type oauthBearer struct {
	ts oauth2.TokenSource
}

func (o *oauthBearer) AuthHeader(_ context.Context) (string, string, error) {
	tok, err := o.ts.Token()
	if err != nil {
		return "", "", fmt.Errorf("oauth2 token: %w", err)
	}
	return "Authorization", "Bearer " + tok.AccessToken, nil
}

// randID 生成响应里的 chatcmpl-XXX 后缀（OpenAI 格式约定）。
func randID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
