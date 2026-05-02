// Package filter 定义端点选择的过滤器接口与各类 Filter 实现。
//
// 内置 Filter：Cooldown / Group / Health / PrefixCache / Busy / Rps/Tpm/Rpm。
package filter

import (
	"context"

	"github.com/zereker-labs/ai-gateway/pkg/ctx"
	"github.com/zereker-labs/ai-gateway/pkg/schedule"
)

// Filter 调度链的过滤器；从候选池中淘汰不可用 endpoint。
type Filter interface {
	Name() string
	Filter(c context.Context, in schedule.PickInput, eps []*ctx.Endpoint) (kept []*ctx.Endpoint, rec ctx.FilterRecord)
}

// Scorer 在 Filter 之上加打分能力（如 PrefixCacheScheduler）。
type Scorer interface {
	Filter
	Score(c context.Context, in schedule.PickInput, eps []*ctx.Endpoint) []ScoredEndpoint
}

// ScoredEndpoint 打分结果；越大越优先（影响加权随机）。
type ScoredEndpoint struct {
	Endpoint *ctx.Endpoint
	Score    float64
}
