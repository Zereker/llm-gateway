package schedule

import (
	"context"
	"time"

	"github.com/zereker-labs/ai-gateway/pkg/ctx"
)

// HealthChecker 健康检查器接口。
//
// 自部署：合并主动 probe + 被动 fail count；
// 厂商：仅被动 fail count。
type HealthChecker interface {
	IsHealthy(c context.Context, ep *ctx.Endpoint) bool
}

// HealthProber 主动 probe（仅 FormSelfHosted）。独立 goroutine 周期性 GET。
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
