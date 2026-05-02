package ratelimit

import "github.com/zereker-labs/ai-gateway/pkg/domain"

// ConfigStore 限流配置查询接口（用户层 / 模型层 / endpoint 层阈值）。
//
// 默认实现走 pkg/config.Store + 内存 LRU 缓存；
// 详见 docs/architecture/06-pluggable-infra.md。
//
// 限流的原子计数（INCR + 比较）由 Checker 实现自己定义底层 KV / Lua 抽象；
// 不在本接口。
//
// Implementations MUST be safe for concurrent use（M6 + Checker 多 goroutine 同时调用）。
type ConfigStore interface {
	GetAPIKeyLimit(apiKeyID, serviceID string) *domain.LayerSpec
	GetUserLimit(userID, serviceID string) *domain.LayerSpec
	GetServiceDefaultUserLimit(serviceID string) *domain.LayerSpec
	GetServiceLimit(serviceID string) *domain.LayerSpec   // 模型层硬上限
	GetEndpointLimit(endpointID string) *domain.LayerSpec // endpoint 层硬上限
}

// Config ConfigStore 中限流相关配置的统一形态（运维 / Admin 用）。
type Config struct {
	APIKey  map[string]map[string]domain.LayerSpec // /ratelimit/apikey/{api_key_id}/{service_id}
	User    map[string]map[string]domain.LayerSpec // /ratelimit/user/{user_id}/{service_id}
	Service map[string]ServiceLimits            // /ratelimit/service/{service_id}
}

// ServiceLimits 模型级限流配置。
type ServiceLimits struct {
	Model       domain.LayerSpec // 模型层硬上限
	DefaultUser domain.LayerSpec // 该模型下用户层默认值（四级查询链的第三级）
}
