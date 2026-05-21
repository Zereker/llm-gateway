package selector

import (
	"context"

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
		bs := ratelimit.EndpointReserveBuckets(ep)
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

// 编译期断言。
var _ Filter = (*LimitReadFilter)(nil)
