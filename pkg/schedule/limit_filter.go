package schedule

import (
	"context"
	"fmt"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
)

// LimitReadFilter 用 SnapshotBatch 做**只读**过滤（docs/04 §5 §10）：
// 检查每个候选 endpoint 的 quota 是否还有余量；超限的剔除。
//
// **关键约束**：filter 阶段**不**做 ReserveBatch（不能在所有候选 endpoint 上扣减）；
// 真正的 reserve 在 M7 选中 endpoint 之后单独做（避免不被选中的 endpoint 被多扣）。
//
// **Fail-open on Redis error**（docs/04 §8）：endpoint quota read-only filter 不能
// 因 Redis 故障变成硬依赖；故障时保留所有候选，让请求继续 try。
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

	// 把所有候选的 RPM/RPS bucket 平铺，一次 SnapshotBatch 全部查
	type slot struct{ epIdx int }
	var slots []slot
	var allBuckets []ratelimit.Bucket
	for i, ep := range candidates {
		bs := buildEndpointReserveBuckets(ep, 1)
		for _, b := range bs {
			allBuckets = append(allBuckets, b)
			slots = append(slots, slot{epIdx: i})
		}
	}
	if len(allBuckets) == 0 {
		// 候选 endpoint 都没配 quota → 全保留
		return candidates
	}

	states, err := f.store.SnapshotBatch(ctx, allBuckets)
	if err != nil {
		// fail-open：Redis 错时保留所有候选（docs/04 §8）
		metric.Inc(metric.RateLimitFailOpenTotal, "scope", "endpoint", "dimension", "any")
		return candidates
	}

	// 标记超限的 ep
	exhausted := make(map[int]bool, len(candidates))
	for i, st := range states {
		if st.Used+1 > st.Limit { // 已用满
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

// =============================================================================
// Endpoint bucket 构造
// =============================================================================

// buildEndpointReserveBuckets 把 endpoint quota 展开成 RPM + RPS bucket（前扣用）。
//
// **不**包含 TPM——TPM 走 ChargeBatch 后扣（docs/04 §10）。
func buildEndpointReserveBuckets(ep *domain.Endpoint, rpsCost uint32) []ratelimit.Bucket {
	var buckets []ratelimit.Bucket
	q := ep.Quota
	if q.RPM != nil && *q.RPM > 0 {
		buckets = append(buckets, ratelimit.Bucket{
			Key:    fmt.Sprintf("rl:endpoint:%d:rpm", ep.ID),
			Limit:  *q.RPM,
			Cost:   1,
			Window: time.Minute,
		})
	}
	if q.RPS != nil && *q.RPS > 0 {
		buckets = append(buckets, ratelimit.Bucket{
			Key:    fmt.Sprintf("rl:endpoint:%d:rps", ep.ID),
			Limit:  *q.RPS,
			Cost:   1,
			Window: time.Second,
		})
	}
	return buckets
}

// EndpointReserveBuckets 给 M7 用：选中 endpoint 后构造 RPM/RPS reserve buckets（docs/04 §10）。
func EndpointReserveBuckets(ep *domain.Endpoint) []ratelimit.Bucket {
	return buildEndpointReserveBuckets(ep, 1)
}

// EndpointTPMChargeBucket 给 M7 用：成功响应后 charge endpoint TPM bucket。
//
// cost 由调用方传入 rc.Usage.Total。endpoint 没配 TPM 时返 nil。
func EndpointTPMChargeBucket(ep *domain.Endpoint, cost uint32) *ratelimit.Bucket {
	q := ep.Quota
	if q.TPM == nil || *q.TPM == 0 || cost == 0 {
		return nil
	}
	return &ratelimit.Bucket{
		Key:    fmt.Sprintf("rl:endpoint:%d:tpm", ep.ID),
		Limit:  *q.TPM,
		Cost:   cost,
		Window: time.Minute,
	}
}

// EndpointTPMBucketKeys 已废弃：旧 M6 用，新模型 endpoint TPM 在 M7/M10 post-side 直接 ChargeBatch。
// 保留 stub 兼容旧 caller。
func EndpointTPMBucketKeys(ep *domain.Endpoint) []string {
	if ep == nil || ep.Quota.TPM == nil || *ep.Quota.TPM == 0 {
		return nil
	}
	return []string{fmt.Sprintf("rl:endpoint:%d:tpm", ep.ID)}
}

// 编译期断言。
var _ Filter = (*LimitReadFilter)(nil)
