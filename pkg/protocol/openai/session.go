package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// session is the **slim version**: it only handles the HTTP layer (URL +
// auth + Content-Type).
//
// "Protocol layer" tasks like SSE parsing / usage extraction /
// stream_options.include_usage injection have moved to
// pkg/translator/identity.openaiTranslator + openaiResponseHandler.
type session struct {
	ctx context.Context
	ep  *domain.Endpoint
	env *domain.RequestEnvelope

	closed bool
}

func newSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) *session {
	return &session{ctx: c, ep: ep, env: env}
}

// BuildRequest builds an *http.Request:
//   - URL: ep.Routing.URL (by convention holds the full chat completions endpoint)
//   - copies extraHeaders first (the final headers produced by quirks)
//   - then writes Authorization / Content-Type — protocol-required headers are
//     written **last, overriding** quirks, so the deployer can't accidentally
//     break the request by misconfiguring Authorization
//   - Body: already processed by translator + quirks
//
// **Vendor validation**: this Adapter is reused for all OpenAI-compatible
// vendors (openai/ark/deepseek/...), which all use Bearer auth. So
// ep.Auth.Type must be AuthTypeBearer; other types (x-api-key for anthropic /
// aws-sigv4 for bedrock) should go through their own dedicated adapter.
func (s *session) BuildRequest(body []byte, extraHeaders http.Header) (*http.Request, error) {
	if s.ep.Routing.URL == "" {
		return nil, errors.New("openai: ep.routing.url empty")
	}
	if s.ep.Auth.Type != domain.AuthTypeBearer {
		return nil, fmt.Errorf("openai: unsupported auth type %q (want %q)", s.ep.Auth.Type, domain.AuthTypeBearer)
	}
	bearer, err := domain.DecodePayload[domain.BearerAuth](s.ep.Auth)
	if err != nil {
		return nil, fmt.Errorf("openai: decode bearer auth: %w", err)
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
	if bearer.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+bearer.APIKey)
	}
	return req, nil
}

// Close releases resources; idempotent.
func (s *session) Close() error {
	s.closed = true
	return nil
}

// Compile-time assertion.
var _ protocol.Session = (*session)(nil)
