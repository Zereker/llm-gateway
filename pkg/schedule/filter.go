package schedule

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// Filter 是 Filter 链的单元；输入候选 → 输出（缩减后的）候选。
//
// 实现 MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
//
// **内置 filters（v0.5）**：
//   - cooldown        排除冷却中候选（CooldownFilter）
//   - limit_read      排除 endpoint quota 超限候选（LimitReadFilter）
//   - weighted_random 按 weight 概率选 1 个（WeightedRandomSelector，必须放最后）
//
// 加新 filter：实现 Filter 接口；cmd 装配时按 cfg.Scheduler.Filters 顺序 wire。
type Filter interface {
	// Name 用于 cfg.Scheduler.Filters 的 string 匹配 + log/metric 标签。
	Name() string

	// Apply 输入候选 + 请求上下文 → 输出筛后候选。
	// 返回空切片 = 全过滤掉（M7 driver loop 会 abort 503）。
	Apply(ctx context.Context, candidates []*domain.Endpoint, req *Request) []*domain.Endpoint
}

// runChain 顺序应用 filter 链；任一 filter 返回空切片即提前退出。
//
// 不并行——filter 之间有依赖（cooldown 在 limit_read 之前更省 Redis call）。
func runChain(ctx context.Context, filters []Filter, candidates []*domain.Endpoint, req *Request) []*domain.Endpoint {
	for _, f := range filters {
		candidates = f.Apply(ctx, candidates, req)
		if len(candidates) == 0 {
			return nil
		}
	}
	return candidates
}
