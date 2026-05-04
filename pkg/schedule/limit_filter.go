package schedule

import (
	"context"
	"fmt"
	"time"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/ratelimit"
)

// LimitReadFilter 用 ratelimit.Store 检查每个候选的 endpoint quota；超限的排除。
//
// 跟 user 维度的 M6 RateLimit 区别：
//   - M6：用户视角"这个用户该请求吗"；任一桶超限 → 429（用户违规）
//   - 此 filter：endpoint 视角"这个 endpoint 还能接吗"；超限 → 排除候选；全排完 → 503（容量问题）
//
// **bucket 命名**：`rl:endpoint:<id>:<dim>`（rpm/tpm/rps）
//
// **TPM 估值**：req.TPMCost 来自 M6 EnsureTPMEstimate（input chars/4 + max_tokens）；
// 跟 M6 user 维度共用同一估值，调账时由 M10 一起 AdjustBatch。
//
// **TPM 调账 keys**：filter 在 reserve 成功后，把命中的 TPM bucket key 追加到
// rc.RateLimit.TPMBucketKeys（M7 driver 调用时传过来；这里通过 callback）。
//
// 不在 filter 接口里塞 callback 太丑——简化做法：filter 只做检查；调账 keys 收集
// 由 M7 在 Pick 之后基于选中 ep 自己算。
//
// **Redis 错误处理**：fail-open（让请求通过）。endpoint quota 不是用户违规，
// Redis 临时故障时容忍多打几个请求；准确性 vs 可用性的 trade-off。
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

	out := make([]*domain.Endpoint, 0, len(candidates))
	for _, ep := range candidates {
		buckets := buildEndpointBuckets(ep, req.TPMCost)
		if len(buckets) == 0 {
			// endpoint 没配 quota → 不限，直接保留
			out = append(out, ep)
			continue
		}
		allowed, _, err := f.store.ReserveBatch(ctx, buckets)
		if err != nil {
			// fail-open：Redis 错时保留候选
			out = append(out, ep)
			continue
		}
		if allowed {
			out = append(out, ep)
		}
		// 超限就跳；不加进 out
	}
	return out
}

// EndpointTPMBucketKeys 给 M7 用：选中某个 endpoint 后，拿它的 TPM bucket keys
// 追加到 rc.RateLimit.TPMBucketKeys（M10 commit 时一起 adjust）。
//
// 没在 LimitReadFilter.Apply 里直接写 rc 是因为 filter 不应有 rc 副作用——
// 选中 ep 后由 M7 driver 显式调本函数。
func EndpointTPMBucketKeys(ep *domain.Endpoint) []string {
	q := ep.Quota
	if q.TPM == nil || *q.TPM == 0 {
		return nil
	}
	return []string{fmt.Sprintf("rl:endpoint:%d:tpm", ep.ID)}
}

// buildEndpointBuckets 把 ep.Quota 展开成 ratelimit.Bucket 列表。
//
// 跟 user 维度不同：endpoint 没有 per_model 概念（endpoint 已经是 model 维度的实体）；
// 只有一组 quota。
func buildEndpointBuckets(ep *domain.Endpoint, tpmCost uint32) []ratelimit.Bucket {
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
	if q.TPM != nil && *q.TPM > 0 {
		buckets = append(buckets, ratelimit.Bucket{
			Key:    fmt.Sprintf("rl:endpoint:%d:tpm", ep.ID),
			Limit:  *q.TPM,
			Cost:   tpmCost,
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

// 编译期断言。
var _ Filter = (*LimitReadFilter)(nil)
