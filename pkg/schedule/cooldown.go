package schedule

import "github.com/zereker-labs/ai-gateway/pkg/domain"

// CooldownManager 端点失败隔离器。
//
// 限流（M6 失败回写）和调度（M7 RetryExecutor）共用此抽象；
// 详见 docs/architecture/03-endpoint-scheduling.md 第 7 节。
//
// Implementations MUST be safe for concurrent use（M7 多 goroutine 同时 OnFailure / IsCooldown）。
type CooldownManager interface {
	OnFailure(epID string, class domain.ErrorClass)
	IsCooldown(epID string) bool
	Clear(epID string)
}
