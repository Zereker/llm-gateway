package gemini

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// session is a **slim version**: it only handles the HTTP layer (URL + auth header).
//
// Body translation / response translation / usage extraction all live in
// internal/translator/openai_gemini.translator.
//
// The three auth shapes are unified behind newTokenProvider (gemini-key / vertex-adc /
// oauth2-sa); the session doesn't know the concrete type.
//
// **Streaming**: when streaming=true (client sent stream:true, determined by Factory
// from the raw body), the URL is swapped to :streamGenerateContent?alt=sse and the
// upstream returns a Gemini SSE stream; the shape translation (Gemini SSE chunk ->
// OpenAI SSE chunk) happens in the openai_gemini responseHandler.
type session struct {
	ctx       context.Context
	ep        *domain.Endpoint
	tp        tokenProvider
	streaming bool

	closed bool
}

func newSession(c context.Context, ep *domain.Endpoint, tp tokenProvider, streaming bool) *session {
	return &session{ctx: c, ep: ep, tp: tp, streaming: streaming}
}

// geminiStreamURL swaps the :generateContent endpoint for :streamGenerateContent?alt=sse
// when streaming.
//   - alt=sse makes Gemini return SSE (data: {json}\n\n) instead of the default JSON
//     array framing.
//   - Already a :streamGenerateContent endpoint -> only append alt=sse.
//   - Preserve any existing query on base (e.g. ?key=...).
//   - Non-standard URL (no :generateContent found) -> return as-is (the deployer may
//     have already configured the streaming endpoint directly).
func geminiStreamURL(base string) string {
	if strings.Contains(base, ":streamGenerateContent") {
		return ensureAltSSE(base)
	}
	if i := strings.LastIndex(base, ":generateContent"); i >= 0 {
		return ensureAltSSE(base[:i] + ":streamGenerateContent" + base[i+len(":generateContent"):])
	}
	return base
}

func ensureAltSSE(u string) string {
	if strings.Contains(u, "alt=sse") {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "alt=sse"
}

// BuildRequest constructs an *http.Request:
//   - URL: ep.Routing.URL (expected to hold the full :generateContent endpoint); rewritten
//     to :streamGenerateContent?alt=sse when streaming.
//   - Adds the vendor-specific auth header (x-goog-api-key or Authorization: Bearer)
//   - body: already translated by the translator (OpenAI ChatCompletion -> Gemini
//     generateContent)
func (s *session) BuildRequest(body []byte, extraHeaders http.Header) (*http.Request, error) {
	if s.ep.Routing.URL == "" {
		return nil, errors.New("gemini: ep.routing.url empty")
	}
	url := s.ep.Routing.URL
	if s.streaming {
		url = geminiStreamURL(url)
	}
	req, err := http.NewRequestWithContext(s.ctx, "POST", url, bytes.NewReader(body))
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
