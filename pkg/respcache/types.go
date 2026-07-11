package respcache

import "github.com/zereker/llm-gateway/pkg/domain"

// CachedResponse is one complete cached non-streaming response. The value is
// owned by the cache capability so concrete stores do not depend on HTTP
// middleware types.
type CachedResponse struct {
	StatusCode  int           `json:"status_code"`
	ContentType string        `json:"content_type"`
	Body        []byte        `json:"body"`
	Usage       *domain.Usage `json:"usage,omitempty"`
}
