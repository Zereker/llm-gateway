package ratelimit

import "github.com/zereker-labs/ai-gateway/pkg/ctx"

// ConfigStore 限流配置查询接口（用户层 / 模型层 / endpoint 层阈值）。
//
// 默认实现走 pkg/config.Store + 内存 LRU 缓存；
// 详见 docs/architecture/06-pluggable-infra.md。
//
// 限流的原子计数（INCR + 比较）走 pkg/store.Cache.EvalLimit，不在本接口；
// Checker 实现同时持有 ConfigStore + store.Cache。
type ConfigStore interface {
	GetAPIKeyLimit(apiKeyID, serviceID string) *ctx.LayerSpec
	GetUserLimit(userID, serviceID string) *ctx.LayerSpec
	GetServiceDefaultUserLimit(serviceID string) *ctx.LayerSpec
	GetServiceLimit(serviceID string) *ctx.LayerSpec   // 模型层硬上限
	GetEndpointLimit(endpointID string) *ctx.LayerSpec // endpoint 层硬上限
}

// Config ConfigStore 中限流相关配置的统一形态（运维 / Admin 用）。
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
