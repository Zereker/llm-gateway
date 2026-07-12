// Package cohere is the vendor implementation (HTTP layer) for Cohere v2.
//
// Protocol shape translation lives in internal/translator/openai_cohere (endpoint
// protocol set to cohere). This package only handles HTTP: Bearer auth +
// routing.url (the Cohere /v2/chat endpoint).
//
// Wiring: endpoint `vendor: cohere` + `protocol: cohere` + `auth.type: bearer`
// (payload.api_key = Cohere key). internal/builtin.NewLookup wires this package
// plus the openai_cohere translator into the built-in lookup.
package cohere

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// Factory implements protocol.Factory. No custom Classify — Cohere errors fall back
// to DefaultClassifier's status-based classification.
type Factory struct{}

// Metadata returns static metadata.
func (Factory) Metadata() protocol.Metadata {
	return protocol.Metadata{
		Vendor:              "cohere",
		SupportedModalities: []domain.Modality{domain.ModalityChat},
	}
}

// NewSession constructs the session for this request.
func (Factory) NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (protocol.Session, error) {
	return &session{ctx: c, ep: ep}, nil
}

type session struct {
	ctx context.Context
	ep  *domain.Endpoint
}

// BuildRequest: Bearer auth + routing.url.
func (s *session) BuildRequest(body []byte, extraHeaders http.Header) (*http.Request, error) {
	if s.ep.Routing.URL == "" {
		return nil, errors.New("cohere: ep.routing.url empty")
	}

	if s.ep.Auth.Type != domain.AuthTypeBearer {
		return nil, fmt.Errorf("cohere: unsupported auth type %q (want %q)", s.ep.Auth.Type, domain.AuthTypeBearer)
	}

	bearer, err := domain.DecodePayload[domain.BearerAuth](s.ep.Auth)
	if err != nil {
		return nil, fmt.Errorf("cohere: decode bearer: %w", err)
	}

	req, err := http.NewRequestWithContext(s.ctx, "POST", s.ep.Routing.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	for k, vs := range extraHeaders { // quirks first
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	req.Header.Set("Content-Type", "application/json") // then protocol-required (overrides)

	if bearer.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+bearer.APIKey)
	}

	return req, nil
}

// Close is an idempotent no-op.
func (s *session) Close() error { return nil }

var _ protocol.Session = (*session)(nil)
