package ratelimit

import (
	"context"
	"time"

	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/domain"
)

// EndpointQuota 实现 dispatch.EndpointQuota——把 ratelimit.Store + selector
// 提供的 bucket key 派生 helper 包成 dispatch port。
//
// **职责**：
//   - Reserve: 拉 endpoint 的 RPM/RPS bucket 列表，调 store.ReserveBatch；超限返
//     denied Verdict（Class=ClassCapacity, Stage=StageReserve）
//   - Charge: 成功 stream 后用真实 usage.Total 写 TPM bucket（fire-and-forget；
//     超限只标 metric，不阻塞响应）
//
// **store == nil**：构造时退化为 noop——可以"没配 ratelimit 就跑空"。
type EndpointQuota struct {
	store Store
}

// NewEndpointQuota 构造一个 EndpointQuota。store == nil 时 Reserve/Charge 都 noop。
func NewEndpointQuota(store Store) *EndpointQuota {
	return &EndpointQuota{store: store}
}

// Reserve 实现 dispatch.EndpointQuota.Reserve。
func (q *EndpointQuota) Reserve(ctx context.Context, ep *domain.Endpoint) (*dispatch.Verdict, error) {
	if q == nil || q.store == nil || ep == nil {
		return nil, nil
	}
	buckets := EndpointReserveBuckets(ep)
	if len(buckets) == 0 {
		return nil, nil
	}
	allowed, violated, err := q.store.ReserveBatch(ctx, buckets)
	if err != nil {
		v := &dispatch.Verdict{
			Stage:  dispatch.StageReserve,
			Class:  dispatch.ClassCapacity,
			Reason: "endpoint reserve: " + err.Error(),
		}
		return v, nil
	}
	if !allowed {
		key := ""
		if violated != nil {
			key = violated.Key
		}
		v := &dispatch.Verdict{
			Stage:  dispatch.StageReserve,
			Class:  dispatch.ClassCapacity,
			Reason: "endpoint quota exhausted: " + key,
		}
		return v, nil
	}
	return nil, nil
}

// Charge 实现 dispatch.EndpointQuota.Charge——TPM 后扣（fire-and-forget）。
//
// usage.Total <= 0 时 no-op。Charge 自带短 timeout 的 background ctx
// （响应已 stream 完，客户端 ctx 可能 cancel）。
func (q *EndpointQuota) Charge(_ context.Context, ep *domain.Endpoint, usage *domain.Usage) {
	if q == nil || q.store == nil || ep == nil || usage == nil || usage.Total <= 0 {
		return
	}
	b := EndpointTPMChargeBucket(ep, uint32(usage.Total))
	if b == nil {
		return
	}
	bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = q.store.ChargeBatch(bgCtx, []Bucket{*b})
}

// 编译期断言。
var _ dispatch.EndpointQuota = (*EndpointQuota)(nil)
