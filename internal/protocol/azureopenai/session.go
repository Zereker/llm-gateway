package azureopenai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// session only handles the Azure OpenAI HTTP layer (URL + api-version +
// api-key header). Protocol shape (SSE parsing / usage extraction) reuses
// OpenAI's translator + response handler.
type session struct {
	ctx context.Context
	ep  *domain.Endpoint
	env *domain.RequestEnvelope
}

func newSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) *session {
	return &session{ctx: c, ep: ep, env: env}
}

// BuildRequest constructs the *http.Request:
//   - URL: ep.Routing.URL (the full Azure endpoint); if the api-version
//     query is missing and ep.Routing.APIVersion is non-empty, it gets
//     appended.
//   - Auth: `api-key: <key>` (reuses AuthTypeBearer's BearerAuth.APIKey
//     payload).
//   - Header order: quirks first, then protocol-required headers
//     (overriding), to prevent the deployer from accidentally clobbering
//     the auth header.
func (s *session) BuildRequest(body []byte, extraHeaders http.Header) (*http.Request, error) {
	if s.ep.Routing.URL == "" {
		return nil, errors.New("azure-openai: ep.routing.url empty")
	}

	if s.ep.Auth.Type != domain.AuthTypeBearer {
		return nil, fmt.Errorf("azure-openai: unsupported auth type %q (want %q; payload.api_key = Azure key)",
			s.ep.Auth.Type, domain.AuthTypeBearer)
	}

	key, err := domain.DecodePayload[domain.BearerAuth](s.ep.Auth)
	if err != nil {
		return nil, fmt.Errorf("azure-openai: decode auth: %w", err)
	}

	endpoint, err := ensureAPIVersion(s.ep.Routing.URL, s.ep.Routing.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("azure-openai: routing url: %w", err)
	}

	req, err := http.NewRequestWithContext(s.ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	for k, vs := range extraHeaders { // quirks first
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	req.Header.Set("Content-Type", "application/json") // then protocol-required (overriding)

	if key.APIKey != "" {
		req.Header.Set("api-key", key.APIKey)
	}

	return req, nil
}

// ensureAPIVersion appends the api-version query if the URL is missing it
// and a version was provided.
func ensureAPIVersion(raw, version string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}

	q := u.Query()
	if q.Get("api-version") == "" && version != "" {
		q.Set("api-version", version)
		u.RawQuery = q.Encode()
	}

	return u.String(), nil
}

// Close is an idempotent no-op (this session holds no resources).
func (s *session) Close() error { return nil }

// Compile-time assertion.
var _ protocol.Session = (*session)(nil)
