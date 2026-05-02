// Package health 定义健康检查器：
// 自部署（双轨：主动 probe + 被动 fail count）；厂商（仅被动）。
package health

import (
	"context"
	"time"

	"github.com/zereker-labs/ai-gateway/pkg/ctx"
)

// Checker 健康检查器接口。
//
// 自部署：合并主动 probe + 被动 fail count；
// 厂商：仅被动 fail count。
type Checker interface {
	IsHealthy(c context.Context, ep *ctx.Endpoint) bool
}

// Prober 主动 probe（仅 FormSelfHosted）。独立 goroutine 周期性 GET。
type Prober interface {
	Start(c context.Context)
	LastResult(epID string) (Result, bool)
}

// Result 主动 probe 的最近一次结果。
type Result struct {
	Healthy bool
	At      time.Time
	Latency time.Duration
}
