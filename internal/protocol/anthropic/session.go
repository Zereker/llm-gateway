package anthropic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// **Version header required by the Anthropic API**: deployer doesn't need to configure it;
// the adapter adds it automatically. Update here when upgrading the API version.
const anthropicAPIVersion = "2023-06-01"

// session is a **slim** implementation: it only handles the HTTP layer (URL + auth +
// required headers).
//
// Body translation / response translation / usage extraction all live in
// internal/translator/openai_anthropic.translator.
type session struct {
	ctx context.Context
	ep  *domain.Endpoint

	closed bool
}

func newSession(c context.Context, ep *domain.Endpoint) *session {
	return &session{ctx: c, ep: ep}
}

// BuildRequest constructs an *http.Request:
//   - URL: ep.Routing.URL (by convention holds the full /v1/messages endpoint)
//   - x-api-key: decoded from ep.Auth.Payload (XAPIKeyAuth)
//   - anthropic-version: hard-coded (required by the API)
//   - body: already translated by the translator (OpenAI ChatCompletion → Anthropic Messages)
//
// **Vendor validation**: this Adapter uses x-api-key auth; any other auth type is rejected
// outright (pointing the caller to the correct vendor).
func (s *session) BuildRequest(body []byte, extraHeaders http.Header) (*http.Request, error) {
	if s.ep.Routing.URL == "" {
		return nil, errors.New("anthropic: ep.routing.url empty")
	}
	if s.ep.Auth.Type != domain.AuthTypeXAPIKey {
		return nil, fmt.Errorf("anthropic: unsupported auth type %q (want %q)", s.ep.Auth.Type, domain.AuthTypeXAPIKey)
	}
	apikey, err := domain.DecodePayload[domain.XAPIKeyAuth](s.ep.Auth)
	if err != nil {
		return nil, fmt.Errorf("anthropic: decode x-api-key: %w", err)
	}
	if apikey.APIKey == "" {
		return nil, errors.New("anthropic: x-api-key empty")
	}

	req, err := http.NewRequestWithContext(s.ctx, "POST", s.ep.Routing.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// quirks headers first
	for k, vs := range extraHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// then protocol-required headers (override)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apikey.APIKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	return req, nil
}

func (s *session) Close() error {
	s.closed = true
	return nil
}

// Compile-time assertion.
var _ protocol.Session = (*session)(nil)
