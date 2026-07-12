package selector

import (
	"context"

	"github.com/zereker/llm-gateway/internal/domain"
)

// BusyMetricProvider looks up an endpoint's current busy score (0.0-1.0; higher = busier).
//
// **Typical score sources** (self-hosted models):
//   - vLLM / SGLang: KV cache utilization (GPU memory used / total)
//   - custom-built inference: queue depth / max queue length
//   - generic HTTP upstream: active connections / max conn
//
// Implementations MUST be safe for concurrent use (queried concurrently by multiple goroutines).
//
// **Data source convention**:
//   - the score is self-reported by the endpoint (HTTP /metrics endpoint / heartbeat); this
//     interface only consumes the cached score. Provider implementations must refresh asynchronously
//     (scrape interval ~5-10s).
//   - unavailable / endpoint hasn't reported: return 0 (treated as idle, not subject to busy filtering).
//
// **v1.0 minimum**: this interface + the BusyFilter implementation; Provider implementations are
// left to self-hosted deployers to write against their own metric source (community-contrib in nature).
type BusyMetricProvider interface {
	BusyScore(ctx context.Context, endpointID int64) float64
}

// BusyFilter excludes endpoints whose busy score exceeds the threshold.
//
// **Threshold choice**: defaults to 0.85 — most inference frameworks start queuing / rejecting new
// requests at 80%+ KV utilization; 0.85 leaves buffer to route requests to a less loaded instance.
//
// **Degenerate behavior**:
//   - no Provider configured → pass through (treated as v0.5 behavior)
//   - all candidates are busy → still pass through (letting the request reach at least one ep is
//     better than hammering into a 503; this decision is debatable — v1.x could add a strict mode
//     for users to opt into)
//
// **Suggested ordering**: place after cooldown, before selector. Busy != failure, so it doesn't go
// into cooldown, but it also shouldn't be picked; it's just temporarily skipped.
type BusyFilter struct {
	threshold float64
	provider  BusyMetricProvider
}

// NewBusyFilter constructs a filter; uses the default 0.85 when threshold <= 0.
func NewBusyFilter(threshold float64) *BusyFilter {
	if threshold <= 0 {
		threshold = 0.85
	}

	return &BusyFilter{threshold: threshold}
}

// SetProvider wires up the provider. Called during cmd wiring (only cmd knows which provider implementations exist).
func (f *BusyFilter) SetProvider(p BusyMetricProvider) { f.provider = p }

func (f *BusyFilter) Name() string { return "busy" }

// Apply implements Filter.Apply.
func (f *BusyFilter) Apply(ctx context.Context, candidates []*domain.Endpoint, _ *Request) []*domain.Endpoint {
	if f.provider == nil || len(candidates) == 0 {
		return candidates
	}

	live := make([]*domain.Endpoint, 0, len(candidates))
	for _, ep := range candidates {
		if f.provider.BusyScore(ctx, ep.ID) <= f.threshold {
			live = append(live, ep)
		}
	}

	if len(live) == 0 {
		// all busy → still pass through to avoid a 503; let the busiest one bear the load (production should alert early)
		return candidates
	}

	return live
}

// Compile-time assertion.
var _ Filter = (*BusyFilter)(nil)
