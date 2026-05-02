package repo

import (
	"context"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// EndpointProvider M7 Schedule middleware 的依赖：按 model + group 选 endpoint。
//
// v0.1 简化版：直接选第一个匹配的 endpoint，无 Filter / 无 Retry / 无打分。
// v0.5+ 由 pkg/schedule 完整接管（Filter 链 + RetryExecutor + CooldownManager）。
//
// 内置默认实现 KVEndpointProvider 走 pkg/store.KV + 内存缓存。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
type EndpointProvider interface {
	// PickForModel 从匹配 model + group 的 endpoints 中选一个。
	//
	// group 为空时按 "default" 处理。
	// 找不到匹配 endpoint 时返回错误（M7 abort 503）。
	PickForModel(c context.Context, model, group string) (*domain.Endpoint, error)

	// List 返回所有 endpoint（启动诊断 / Admin API 用）。
	List(c context.Context) ([]*domain.Endpoint, error)
}
