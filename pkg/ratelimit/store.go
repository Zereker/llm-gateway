package ratelimit

import (
	"context"

	"github.com/zereker-labs/ai-gateway/pkg/ctx"
)

// Store 限流的底层存储抽象。
//
// 默认实现支持内存（单实例 / 测试）和 Redis（多实例共享）；
// 详见 docs/architecture/06-pluggable-infra.md。
type Store interface {
	// EvalLimit 原子比较 + 自增。incr=0 时仅读 + 比较，不写。
	EvalLimit(c context.Context, key string, capLimit, incr, ttlSec int64) (current int64, blocked bool, err error)

	// 配置查询：从 ConfigStore 加载并缓存。
	GetAPIKeyLimit(apiKeyID, serviceID string) *ctx.LayerSpec
	GetUserLimit(userID, serviceID string) *ctx.LayerSpec
	GetServiceDefaultUserLimit(serviceID string) *ctx.LayerSpec
	GetServiceLimit(serviceID string) *ctx.LayerSpec   // 模型层硬上限
	GetEndpointLimit(endpointID string) *ctx.LayerSpec // endpoint 层硬上限
}

// Config ConfigStore 中限流相关配置的统一形态。
type Config struct {
	APIKey  map[string]map[string]ctx.LayerSpec // /ratelimit/apikey/{api_key_id}/{service_id}
	User    map[string]map[string]ctx.LayerSpec // /ratelimit/user/{user_id}/{service_id}
	Service map[string]ServiceLimits            // /ratelimit/service/{service_id}
}

// ServiceLimits 模型级限流配置。
type ServiceLimits struct {
	Model       ctx.LayerSpec // 模型层硬上限
	DefaultUser ctx.LayerSpec // 该模型下用户层默认值（四级查询链的第三级）
}
