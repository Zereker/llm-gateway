package selector

import (
	"context"

	"github.com/zereker/llm-gateway/internal/domain"
)

// Filter is a unit in the Filter chain; input candidates → output (narrowed) candidates.
//
// Implementations MUST be safe for concurrent use (called concurrently from multiple gin handler goroutines).
//
// **Built-in filters (v0.5)**:
//   - cooldown        excludes candidates that are cooling down (CooldownFilter)
//   - limit_read      excludes candidates over endpoint quota (LimitReadFilter)
//   - weighted_random picks 1 by weighted probability (WeightedRandomPicker, must be placed last)
//
// To add a new filter: implement the Filter interface; cmd wires it in the order given by cfg.Selector.Filters.
type Filter interface {
	// Name is used for string matching in cfg.Selector.Filters + log/metric labels.
	Name() string

	// Apply takes candidates + request context → outputs filtered candidates.
	// Returning an empty slice = everything was filtered out (dispatch goes through
	// FallbackPolicy.OnExhausted; eventually aborts with 503).
	Apply(ctx context.Context, candidates []*domain.Endpoint, req *Request) []*domain.Endpoint
}

// runChain applies the filter chain in order; any filter returning an empty slice exits early.
//
// Not parallel — filters have dependencies between them (cooldown before limit_read saves Redis calls).
func runChain(ctx context.Context, filters []Filter, candidates []*domain.Endpoint, req *Request) []*domain.Endpoint {
	for _, f := range filters {
		candidates = f.Apply(ctx, candidates, req)
		if len(candidates) == 0 {
			return nil
		}
	}

	return candidates
}
