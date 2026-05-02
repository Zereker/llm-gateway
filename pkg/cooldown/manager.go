// Package cooldown 定义端点失败隔离器。
//
// 限流和调度共用此包；详见 docs/architecture/03-endpoint-scheduling.md 第 7 节。
package cooldown

import "github.com/zereker-labs/ai-gateway/pkg/ctx"

// Manager 端点失败隔离器。
type Manager interface {
	OnFailure(epID string, class ctx.ErrorClass)
	IsCooldown(epID string) bool
	Clear(epID string)
}
