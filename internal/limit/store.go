package limit

import "context"

// Store 限流的底层存储抽象。
//
// 默认实现支持内存（单实例 / 测试）和 Redis（多实例共享）；
// 详见 docs/architecture/06-pluggable-infra.md。
type Store interface {
	// EvalLimit 原子比较 + 自增。incr=0 时仅读 + 比较，不写。
	EvalLimit(ctx context.Context, key string, cap, incr, ttlSec int64) (current int64, blocked bool, err error)

	// 配置查询：从 ConfigStore 加载并缓存。
	GetAPIKeyLimit(apiKeyID, serviceID string) *LayerSpec
	GetUserLimit(userID, serviceID string) *LayerSpec
	GetServiceDefaultUserLimit(serviceID string) *LayerSpec
	GetServiceLimit(serviceID string) *LayerSpec   // 模型层硬上限
	GetEndpointLimit(endpointID string) *LayerSpec // endpoint 层硬上限
}

// LimitConfig ConfigStore 中限流相关配置的统一形态。
//
// 各字段独立放置在不同 ConfigStore 路径下，按需 Watch / 加载。
type LimitConfig struct {
	APIKey  map[string]map[string]LayerSpec // /ratelimit/apikey/{api_key_id}/{service_id}
	User    map[string]map[string]LayerSpec // /ratelimit/user/{user_id}/{service_id}
	Service map[string]ServiceLimits        // /ratelimit/service/{service_id}
}

// ServiceLimits 模型级限流配置。
type ServiceLimits struct {
	Model       LayerSpec // 模型层硬上限
	DefaultUser LayerSpec // 该模型下用户层默认值（四级查询链的第三级）
}
