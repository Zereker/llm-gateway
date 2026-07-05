package selector

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
)

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
	store ratelimit.Store
}

func NewLimitReadFilter(store ratelimit.Store) *LimitReadFilter {
	return &LimitReadFilter{store: store}
}

func (f *LimitReadFilter) Name() string { return "limit_read" }

func (f *LimitReadFilter) Apply(ctx context.Context, candidates []*domain.Endpoint, req *Request) []*domain.Endpoint {
	if len(candidates) == 0 || f.store == nil {
		return candidates
	}

	// Flatten all candidates' RPM/RPS buckets and query them all in one SnapshotBatch
	type slot struct{ epIdx int }
	var slots []slot
	var allBuckets []ratelimit.Bucket
	for i, ep := range candidates {
		bs := ratelimit.EndpointReserveBuckets(ep)
		for _, b := range bs {
			allBuckets = append(allBuckets, b)
			slots = append(slots, slot{epIdx: i})
		}
	}
	if len(allBuckets) == 0 {
		// none of the candidate endpoints have quota configured → keep all
		return candidates
	}

	states, err := f.store.SnapshotBatch(ctx, allBuckets)
	if err != nil {
		// fail-open: keep all candidates when Redis errors (docs/04 §8)
		metric.Inc(metric.RateLimitFailOpenTotal, "scope", "endpoint", "dimension", "any")
		return candidates
	}

	// mark endpoints over the limit
	exhausted := make(map[int]bool, len(candidates))
	for i, st := range states {
		if st.Used+1 > st.Limit { // already fully used
			exhausted[slots[i].epIdx] = true
		}
	}
	out := make([]*domain.Endpoint, 0, len(candidates))
	for i, ep := range candidates {
		if !exhausted[i] {
			out = append(out, ep)
		}
	}
	return out
}

// Compile-time assertion.
var _ Filter = (*LimitReadFilter)(nil)
