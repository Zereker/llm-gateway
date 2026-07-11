package selector

import (
	"context"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/metric"
)

// CapacityReader provides a read-only view of endpoint quota availability.
// Selector owns this minimal port and has no knowledge of rate-limit buckets
// or their storage representation.
type CapacityReader interface {
	Available(ctx context.Context, endpoints []*domain.Endpoint) (map[int64]bool, error)
}

// LimitReadFilter uses SnapshotBatch to do **read-only** filtering (docs/04 §5 §10):
// check whether each candidate endpoint's quota still has headroom; endpoints over the limit are excluded.
//
// **Key constraint**: the filter stage does **not** do ReserveBatch (it must not deduct from every
// candidate endpoint); the actual reserve happens after the dispatcher's Pick, via EndpointQuota.Reserve
// (avoiding over-deducting endpoints that weren't even selected).
//
// **Fail-open on Redis error** (docs/04 §8): the endpoint quota read-only filter must not become a
// hard dependency due to a Redis outage; on failure it keeps all candidates, letting the request keep trying.
type LimitReadFilter struct {
	reader CapacityReader
}

func NewLimitReadFilter(reader CapacityReader) *LimitReadFilter {
	return &LimitReadFilter{reader: reader}
}

func (f *LimitReadFilter) Name() string { return "limit_read" }

func (f *LimitReadFilter) Apply(ctx context.Context, candidates []*domain.Endpoint, req *Request) []*domain.Endpoint {
	if len(candidates) == 0 || f.reader == nil {
		return candidates
	}
	hasQuota := false
	for _, candidate := range candidates {
		if candidate != nil && ((candidate.Quota.RPM != nil && *candidate.Quota.RPM > 0) ||
			(candidate.Quota.RPS != nil && *candidate.Quota.RPS > 0)) {
			hasQuota = true
			break
		}
	}
	if !hasQuota {
		return candidates
	}

	available, err := f.reader.Available(ctx, candidates)
	if err != nil {
		// fail-open: keep all candidates when Redis errors (docs/04 §8)
		metric.Inc(metric.RateLimitFailOpenTotal, "scope", "endpoint", "dimension", "any")
		return candidates
	}

	out := make([]*domain.Endpoint, 0, len(candidates))
	for _, ep := range candidates {
		if ok, known := available[ep.ID]; !known || ok {
			out = append(out, ep)
		}
	}
	return out
}

// Compile-time assertion.
var _ Filter = (*LimitReadFilter)(nil)
