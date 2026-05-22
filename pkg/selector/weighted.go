package selector

import (
	"context"
	"math/rand"
)

// Selector Filter / Scorer 之后的最终选择步骤：按 EffectiveWeight 选 1 个候选。
//
// 设计精神（docs/03 §4 §8）：
//   - WeightedRandom 必须基于 Candidate.EffectiveWeight 而非 Endpoint.Weight
//   - 候选只剩 1 时 trivially 返回该候选
//   - 全部 EffectiveWeight=0 或空 → 返回 nil（dispatch.FallbackPolicy.OnExhausted 兜底）
//
// 实现 MUST be safe for concurrent use。
type Picker interface {
	Select(ctx context.Context, candidates []Candidate) *Candidate
}

// WeightedRandomPicker 按 EffectiveWeight 概率分布选 1。
//
// weight=0 → 排除（管理上的"软下线"语义）。
// 全 0 → 返回 nil。
type WeightedRandomPicker struct {
	rng *rand.Rand // nil = 用 math/rand 全局
}

// NewWeightedRandomPicker 构造一个 selector。
//
// rng=nil 时用 math/rand 全局（thread-safe，Go 1.20+ 自动 per-goroutine seed）。
func NewWeightedRandomPicker() *WeightedRandomPicker {
	return &WeightedRandomPicker{}
}

// Select 按 EffectiveWeight 加权随机选 1。
func (s *WeightedRandomPicker) Select(_ context.Context, candidates []Candidate) *Candidate {
	if len(candidates) == 0 {
		return nil
	}
	var total float64
	live := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.EffectiveWeight <= 0 {
			continue
		}
		total += c.EffectiveWeight
		live = append(live, c)
	}
	if len(live) == 0 || total == 0 {
		return nil
	}
	if len(live) == 1 {
		return &live[0]
	}
	target := s.randFloat() * total
	var acc float64
	for i := range live {
		acc += live[i].EffectiveWeight
		if target < acc {
			return &live[i]
		}
	}
	// 数学保证不会到这里，fallback
	return &live[len(live)-1]
}

func (s *WeightedRandomPicker) randFloat() float64 {
	if s.rng != nil {
		return s.rng.Float64()
	}
	return rand.Float64()
}
