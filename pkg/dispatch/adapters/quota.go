package adapters

import (
	"context"
	"time"

	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
)

// EndpointQuotaAdapter 实现 dispatch.EndpointQuota——把 ratelimit.Store + 端点
// bucket key 派生 helper 包成 dispatch port。
//
// **职责**：
//   - Reserve: 拉 endpoint 的 RPM/RPS bucket 列表，调 store.ReserveBatch；超限返
//     QuotaVerdict（Class=ClassCapacity）
//   - ChargeUsage: 成功 stream 后用真实 usage.Total 写 TPM bucket（fire-and-forget；
//     超限只标 metric，不阻塞响应）
//
// **store == nil**：构造时退化为 noop——可以"没配 ratelimit 就跑空"。
type EndpointQuotaAdapter struct {
	store ratelimit.Store
}

// NewEndpointQuota 构造一个 EndpointQuotaAdapter。store == nil 时 Reserve / ChargeUsage 都 noop。
func NewEndpointQuota(store ratelimit.Store) *EndpointQuotaAdapter {
	return &EndpointQuotaAdapter{store: store}
}

// Reserve 实现 dispatch.EndpointQuota.Reserve。
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
		return &dispatch.QuotaVerdict{
			Class:  dispatch.ClassCapacity,
			Reason: "endpoint reserve: " + err.Error(),
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

// ChargeUsage 实现 dispatch.EndpointQuota.ChargeUsage——TPM 后扣（fire-and-forget）。
//
// usage.Total <= 0 时 no-op。自带短 timeout 的 background ctx
// （响应已 stream 完，客户端 ctx 可能 cancel）。
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

// 编译期断言。
var _ dispatch.EndpointQuota = (*EndpointQuotaAdapter)(nil)
