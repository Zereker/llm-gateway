package schedule

import (
	"context"
	"math/rand"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// WeightedRandomSelector Filter 链最后的选 1 步：按 endpoint.weight 概率分布选 1 个。
//
// **weight=0 → 排除**（管理上的"软下线"语义）。
// **全 0 → 排除**（返回空切片，触发 M7 abort 503）。
// **正常 case**：按 weights 比例分布概率选；高 weight 的更可能被选中。
//
// 不并发——每次 Apply 用本地 rand source；但 math/rand 全局 source 是 thread-safe，
// 单 goroutine 调用没问题。
//
// **必须放 Filter 链最后一个**——前面的 filter 只缩减候选；本 selector 把多个
// 缩到 1 个。如果链里再有别的 filter 在它后面，会拿单元素切片继续筛，可能再变 0。
type WeightedRandomSelector struct {
	rng *rand.Rand // nil = 用 math/rand 全局
}

// NewWeightedRandomSelector 构造一个 selector。
//
// rng=nil 时用 math/rand 全局（thread-safe，Go 1.20+ 自动 per-goroutine seed）。
func NewWeightedRandomSelector() *WeightedRandomSelector {
	return &WeightedRandomSelector{}
}

func (s *WeightedRandomSelector) Name() string { return "weighted_random" }

func (s *WeightedRandomSelector) Apply(_ context.Context, candidates []*domain.Endpoint, _ *Request) []*domain.Endpoint {
	if len(candidates) == 0 {
		return nil
	}
	// 计算 weight 总和；同时排除 weight=0 的
	var total uint32
	live := make([]*domain.Endpoint, 0, len(candidates))
	for _, ep := range candidates {
		if ep.Weight == 0 {
			continue
		}
		total += ep.Weight
		live = append(live, ep)
	}
	if len(live) == 0 || total == 0 {
		return nil
	}

	// 在 [0, total) 之间取一个随机点；按 prefix sum 找到对应 endpoint
	target := uint32(s.randInt63() % int64(total))
	var acc uint32
	for _, ep := range live {
		acc += ep.Weight
		if target < acc {
			return []*domain.Endpoint{ep}
		}
	}
	// 不会到这（数学保证），fallback
	return []*domain.Endpoint{live[len(live)-1]}
}

func (s *WeightedRandomSelector) randInt63() int64 {
	if s.rng != nil {
		return s.rng.Int63()
	}
	return rand.Int63()
}

// 编译期断言。
var _ Filter = (*WeightedRandomSelector)(nil)
