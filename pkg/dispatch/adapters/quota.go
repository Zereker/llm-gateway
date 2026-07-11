package adapters

import (
	"context"
	"time"

	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
)

// EndpointQuotaAdapter implements dispatch.EndpointQuota — wraps
// ratelimit.Store plus the endpoint bucket-key derivation helpers as a
// dispatch port.
//
// **Responsibilities**:
//   - Reserve: fetches the endpoint's RPM/RPS bucket list and calls
//     store.ReserveBatch; returns a QuotaVerdict (Class=ClassCapacity) when
//     over limit
//   - ChargeUsage: after a successful stream, writes the TPM bucket using the
//     real usage.Total (fire-and-forget; going over limit only marks a
//     metric, it never blocks the response)
//
// **store == nil**: degrades to a no-op at construction time — lets the
// gateway run fine with ratelimit unconfigured.
type EndpointQuotaAdapter struct {
	store ratelimit.Store
}

// NewEndpointQuota constructs an EndpointQuotaAdapter. When store == nil,
// both Reserve and ChargeUsage are no-ops.
func NewEndpointQuota(store ratelimit.Store) *EndpointQuotaAdapter {
	return &EndpointQuotaAdapter{store: store}
}

// Reserve implements dispatch.EndpointQuota.Reserve.
func (q *EndpointQuotaAdapter) Reserve(ctx context.Context, ep *domain.Endpoint) (*dispatch.QuotaVerdict, error) {
	if q == nil || q.store == nil || ep == nil {
		return nil, nil
	}
	buckets := ratelimit.EndpointReserveBuckets(ep)
	if len(buckets) == 0 {
		return nil, nil
	}
	allowed, violated, err := q.store.ReserveBatch(ctx, buckets)
	if err != nil {
		// **A dependency failure is not a capacity rejection**: a Redis error
		// translates to ClassUnknown — retryable (try the next endpoint), but
		// Scheduler.Report never writes a cooldown for unknown.
		// Translating it to ClassCapacity instead would, on a single Redis
		// blip, push every healthy endpoint on the path into a capacity
		// cooldown, and the contamination would linger for a full TTL even
		// after Redis recovers (docs/04 §8: an endpoint ReserveBatch error
		// means the current endpoint is treated as unavailable and we try the
		// next one — it must not be mistakenly flagged as a bad endpoint).
		return &dispatch.QuotaVerdict{
			Class:  dispatch.ClassUnknown,
			Reason: "endpoint reserve (store error): " + err.Error(),
		}, nil
	}
	if !allowed {
		key := ""
		if violated != nil {
			key = violated.Key
		}
		return &dispatch.QuotaVerdict{
			Class:     dispatch.ClassCapacity,
			BucketKey: key,
		}, nil
	}
	return nil, nil
}

// ChargeUsage implements dispatch.EndpointQuota.ChargeUsage — charges TPM
// after the fact (fire-and-forget).
//
// No-op when usage.Total <= 0. Uses its own short-timeout background ctx
// (the response has already finished streaming, and the client's ctx may
// have been cancelled).
func (q *EndpointQuotaAdapter) ChargeUsage(_ context.Context, ep *domain.Endpoint, usage *domain.Usage) {
	if q == nil || q.store == nil || ep == nil || usage == nil || usage.Total <= 0 {
		return
	}
	b := ratelimit.EndpointTPMChargeBucket(ep, uint32(usage.Total))
	if b == nil {
		return
	}
	bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = q.store.ChargeBatch(bgCtx, []ratelimit.Bucket{*b})
}

// Release implements dispatch.EndpointQuota.Release — rolls back the RPM/RPS
// reserve for an attempt that never reached the endpoint. Best-effort: a
// failure only logs (the request is already failing anyway).
func (q *EndpointQuotaAdapter) Release(ctx context.Context, ep *domain.Endpoint) {
	if q == nil || q.store == nil || ep == nil {
		return
	}
	buckets := ratelimit.EndpointReserveBuckets(ep)
	if len(buckets) == 0 {
		return
	}
	_ = q.store.ReleaseBatch(ctx, buckets)
}

// Compile-time assertion.
var _ dispatch.EndpointQuota = (*EndpointQuotaAdapter)(nil)
