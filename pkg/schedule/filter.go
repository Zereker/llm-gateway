package schedule

import (
	"context"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// Filter 调度链的过滤器；从候选池中淘汰不可用 endpoint。
//
// 内置 Filter：Cooldown / Group / Health / PrefixCache / Busy / Rps/Tpm/Rpm。
type Filter interface {
	Name() string
	Filter(c context.Context, in PickInput, eps []*domain.Endpoint) (kept []*domain.Endpoint, rec domain.FilterRecord)
}

// Scorer 在 Filter 之上加打分能力（如 PrefixCacheScheduler）。
type Scorer interface {
	Filter
	Score(c context.Context, in PickInput, eps []*domain.Endpoint) []ScoredEndpoint
}

// ScoredEndpoint 打分结果；越大越优先（影响加权随机）。
type ScoredEndpoint struct {
	Endpoint *domain.Endpoint
	Score    float64
}
