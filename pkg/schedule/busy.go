package schedule

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// BusyMetricProvider 反查 endpoint 当前 busy score（0.0-1.0；越大越忙）。
//
// **典型 score 来源**（self-hosted 模型）：
//   - vLLM / SGLang：KV cache 利用率（GPU memory 占用 / 总量）
//   - 自研推理：queue depth / max queue length
//   - 一般 HTTP 上游：active connections / max conn
//
// 实现 MUST be safe for concurrent use（多 goroutine 同时查）。
//
// **数据来源约定**：
//   - score 由 endpoint 自己上报（HTTP /metrics 端点 / 心跳）；本 interface 只
//     消费 cached score。Provider 实现里要做异步刷新（scrape 间隔 ~5-10s）。
//   - 拿不到 / endpoint 没上报：返回 0（视为 idle，不参与 busy 过滤）。
//
// **v1.0 minimum**：本 interface + BusyFilter 实现；Provider 实现交给 self-hosted
// 部署方按各自的 metric source 自己写（社区 contrib 性质）。
type BusyMetricProvider interface {
	BusyScore(ctx context.Context, endpointID int64) float64
}

// BusyFilter 排除 busy score 超过 threshold 的 endpoint。
//
// **threshold 选择**：默认 0.85——大多数推理框架在 80%+ KV 利用率开始排队 / 拒新请求；
// 0.85 留 buffer 让请求路到更空的实例。
//
// **退化行为**：
//   - 没配 Provider → 透传（视为 v0.5 行为）
//   - 全部候选都 busy → 仍透传（让请求至少有 ep 可走，hammer 总比 503 强；
//     这个决策可争论，v1.x 加 strict mode 让用户选）
//
// **顺序建议**：放在 cooldown 后、selector 前。busy ≠ failure，不进 cooldown，
// 但也别选；只是临时跳过。
type BusyFilter struct {
	threshold float64
	provider  BusyMetricProvider
}

// NewBusyFilter 构造 filter；threshold ≤ 0 时用默认 0.85。
//
// provider=nil 时 filter 永远透传（开发期方便；生产应该真挂一个）。
func NewBusyFilter(threshold float64) *BusyFilter {
	if threshold <= 0 {
		threshold = 0.85
	}
	return &BusyFilter{threshold: threshold}
}

// SetProvider 装配 provider。装配 cmd 里调（cmd 才知道有哪些 provider 实现）。
func (f *BusyFilter) SetProvider(p BusyMetricProvider) { f.provider = p }

func (f *BusyFilter) Name() string { return "busy" }

// Apply 实现 Filter.Apply。
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
		// 全 busy → 仍透传，避免 503；让最 busy 的硬撑（生产应该早 alert）
		return candidates
	}
	return live
}

// 编译期断言。
var _ Filter = (*BusyFilter)(nil)
