// Package cache owns the response-cache model and storage ports. Concrete
// Redis implementations live in adapter packages; HTTP middleware consumes
// only these interfaces.
package cache

import (
	"context"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// CachedResponse is one complete, non-streaming cached response.
type CachedResponse struct {
	StatusCode  int           `json:"status_code"`
	ContentType string        `json:"content_type"`
	Body        []byte        `json:"body"`
	Usage       *domain.Usage `json:"usage,omitempty"`
}

// Store persists exact response-cache entries.
type Store interface {
	Get(ctx context.Context, key string) (CachedResponse, bool)
	Set(ctx context.Context, key string, resp CachedResponse, ttl time.Duration)
}

// SemanticStore persists vector-indexed response-cache entries.
type SemanticStore interface {
	Lookup(ctx context.Context, namespace string, vec []float32, threshold float64) (CachedResponse, bool)
	Store(ctx context.Context, namespace string, vec []float32, resp CachedResponse, ttl time.Duration)
}
