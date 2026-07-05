package gemini

import (
	"bytes"
	"context"
	"errors"
	"net/http"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// session is a **slim** implementation: it only handles the HTTP layer (URL +
// auth header).
//
// Body translation / response translation / usage extraction all live in
// pkg/translator/openai_gemini.translator.
//
// The three auth forms are unified behind newTokenProvider (gemini-key /
// vertex-adc / oauth2-sa); session doesn't know the concrete type.
type session struct {
	ctx context.Context
	ep  *domain.Endpoint
	tp  tokenProvider

	closed bool
}

func newSession(c context.Context, ep *domain.Endpoint, tp tokenProvider) *session {
	return &session{ctx: c, ep: ep, tp: tp}
}

// BuildRequest constructs the *http.Request:
//   - URL: ep.Routing.URL (by convention, the full :generateContent endpoint)
//   - adds the vendor-specific auth header (x-goog-api-key or Authorization: Bearer)
//   - body: already translated by translator (OpenAI ChatCompletion → Gemini generateContent)
//
// **Streaming**: not supported in v0.5. When the client sends stream=true, the
// openai_gemini translator returns an error at the TranslateRequest stage (the
// adapter is never reached).
func (s *session) BuildRequest(body []byte, extraHeaders http.Header) (*http.Request, error) {
	if s.ep.Routing.URL == "" {
		return nil, errors.New("gemini: ep.routing.url empty")
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

	hdrName, hdrValue, err := s.tp.AuthHeader(s.ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set(hdrName, hdrValue)

	return req, nil
}

func (s *session) Close() error {
	s.closed = true
	return nil
}

// Compile-time assertion.
var _ protocol.Session = (*session)(nil)
