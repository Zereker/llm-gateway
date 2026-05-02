package schedule

import (
	"context"
	"time"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// HealthChecker 健康检查器接口。
//
// 自部署：合并主动 probe + 被动 fail count；
// 厂商：仅被动 fail count。
//
// Implementations MUST be safe for concurrent use（多 goroutine 在 Filter 链中同时调用）。
type HealthChecker interface {
	IsHealthy(c context.Context, ep *domain.Endpoint) bool
}

// HealthProber 主动 probe（仅 FormSelfHosted）。独立 goroutine 周期性 GET。
//
// 实现内部由 Start 启动的 goroutine 写入结果；LastResult 由 HealthChecker 多 goroutine 读。
// Implementations MUST be safe for concurrent LastResult readers + single Start writer。
type HealthProber interface {
	Start(c context.Context)
	LastResult(epID string) (HealthProbeResult, bool)
}

// HealthProbeResult 主动 probe 的最近一次结果。
type HealthProbeResult struct {
	Healthy bool
	At      time.Time
	Latency time.Duration
}
