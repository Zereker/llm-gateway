package scheduling

import (
	"context"
	"time"

	"github.com/zereker-labs/ai-gateway/internal/errs"
)

// Filter 调度链的过滤器；从候选池中淘汰不可用 endpoint。
type Filter interface {
	Name() string
	Filter(ctx context.Context, in PickInput, eps []*Endpoint) (kept []*Endpoint, rec FilterRecord)
}

// Scorer 在 Filter 之上加打分能力（如 PrefixCacheScheduler）。
type Scorer interface {
	Filter
	Score(ctx context.Context, in PickInput, eps []*Endpoint) []ScoredEndpoint
}

// ScoredEndpoint 打分结果；越大越优先（影响加权随机）。
type ScoredEndpoint struct {
	Endpoint *Endpoint
	Score    float64
}

// CooldownManager 端点失败隔离器。
//
// 详见 docs/architecture/03 第 7 节。
type CooldownManager interface {
	OnFailure(epID string, class errs.Class)
	IsCooldown(epID string) bool
	Clear(epID string)
}

// HealthChecker 健康检查器。
//
// 自部署：合并主动 probe + 被动 fail count；
// 厂商：仅被动 fail count。
type HealthChecker interface {
	IsHealthy(ctx context.Context, ep *Endpoint) bool
}

// Prober 主动 probe（仅 FormSelfHosted）。独立 goroutine 周期性 GET。
type Prober interface {
	Start(ctx context.Context)
	LastResult(epID string) (Result, bool)
}

// Result 主动 probe 的最近一次结果。
type Result struct {
	Healthy bool
	At      time.Time
	Latency time.Duration
}
