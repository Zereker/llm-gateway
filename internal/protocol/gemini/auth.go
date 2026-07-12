package gemini

import (
	"context"
	"encoding/json"
	"fmt"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/zereker/llm-gateway/internal/domain"
)

// tokenProvider is Gemini's auth abstraction — three credential forms:
//
//	gemini-key   → AI Studio API key (x-goog-api-key header)
//	vertex-adc   → Application Default Credentials (Authorization: Bearer <oauth2_token>)
//	oauth2-sa    → embedded Service Account JSON (Authorization: Bearer <oauth2_token>)
//
// built once per session; token caching is reused from oauth2.TokenSource's
// built-in behavior (auto-refresh).
type tokenProvider interface {
	// AuthHeader returns the (header_name, header_value) pair to add to the
	// HTTP request. Returns err on failure (e.g. OAuth refresh error).
	AuthHeader(ctx context.Context) (string, string, error)
}

// newTokenProvider constructs the appropriate provider based on auth.type.
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
		// payload may optionally specify scopes; falls back to default if omitted
		var p domain.VertexADCAuth
		if len(auth.Payload) > 0 {
			_ = json.Unmarshal(auth.Payload, &p) // ignore failure, use default scopes
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

		// The deprecation is about accepting credential configs from untrusted
		// sources without validation; this SA JSON comes from our own AES-GCM
		// encrypted endpoints.auth column, written by the deployer.
		//nolint:staticcheck // SA1019: see above — input is deployer-controlled, not untrusted
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

// headerGoogAPIKey is the AI Studio API-key header name; factored out since
// this package's tests assert against this exact header name.
const headerGoogAPIKey = "x-goog-api-key"

// staticAPIKey uses the x-goog-api-key header for AI Studio.
type staticAPIKey struct {
	key string
}

func (s staticAPIKey) AuthHeader(_ context.Context) (string, string, error) {
	return headerGoogAPIKey, s.key, nil
}

// oauthBearer uses Authorization: Bearer <oauth2 access token> for Vertex.
//
// oauth2.TokenSource has built-in caching (go-oauth2 lib); repeated .Token()
// calls on the same source mostly return the cached token, only auto-refreshing
// before it expires.
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
