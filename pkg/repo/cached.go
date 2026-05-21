package repo

import (
	"context"
	"strconv"
	"time"
)

// 本文件给 repo 的 5 个 SQL Reader/Provider 各包一层 TTL 缓存——gateway 启动时
// cmd 拿 cached 版本注入到 middleware / dispatch port。repo 层唯一的缓存策略。
//
// **设计**：
//   - 每个 cached wrapper 嵌一个 sql reader + 一个 TTLCache
//   - 所有 hot path 走 TTLCache.GetOrLoad（miss + singleflight 一步搞定）
//   - cache key 是查询参数（Resolve(creds) 用 api_key_hash；GetByID(id) 用 id；
//     ListForModel(model,group) 用 "model\x00group" 复合 string；等等）
//   - "not found"（loader 返 nil 或没数据）**不**进缓存——loader 通过返回
//     cache=false 显式告诉缓存层。让"刚创建的资源"立即生效，不被 negative cache
//     卡 TTL 时长
//   - 默认参数（capacity / ttl）给个 sensible default；cmd 可以按需调整

// =============================================================================
// CachedAPIKeyProvider — wraps SQLAPIKeyProvider with per-(hash) TTL LRU
// =============================================================================

// CachedAPIKeyProvider 用 TTL LRU 缓存 Resolve 结果。cache key = api_key_hash。
type CachedAPIKeyProvider struct {
	inner *SQLAPIKeyProvider
	cache *TTLCache[string, *UserIdentity]
}

// NewCachedAPIKeyProvider 默认 capacity=10240（支持几千个并发活跃 key）/ ttl=30s。
// metrics 为 nil 时不上报。
func NewCachedAPIKeyProvider(inner *SQLAPIKeyProvider, capacity int, ttl time.Duration, metrics Metrics) *CachedAPIKeyProvider {
	return &CachedAPIKeyProvider{
		inner: inner,
		cache: NewTTLCache[string, *UserIdentity](capacity, ttl).WithMetrics("api_keys", metrics),
	}
}

func (p *CachedAPIKeyProvider) Resolve(ctx context.Context, creds *Credentials) (*UserIdentity, error) {
	if creds == nil || creds.APIKey == "" {
		return p.inner.Resolve(ctx, creds)
	}
	key := HashAPIKey(creds.APIKey)
	return p.cache.GetOrLoad(ctx, key, func(ctx context.Context) (*UserIdentity, bool, error) {
		v, err := p.inner.Resolve(ctx, creds)
		return v, err == nil && v != nil, err
	})
}

// =============================================================================
// CachedModelServiceReader — wraps SQLModelServiceReader with per-model TTL LRU
// =============================================================================

// CachedModelServiceReader 缓存 GetByModel(model) 结果。
type CachedModelServiceReader struct {
	inner *SQLModelServiceReader
	cache *TTLCache[string, *ModelService]
}

// NewCachedModelServiceReader 默认 capacity=256 / ttl=30s。
func NewCachedModelServiceReader(inner *SQLModelServiceReader, capacity int, ttl time.Duration, metrics Metrics) *CachedModelServiceReader {
	return &CachedModelServiceReader{
		inner: inner,
		cache: NewTTLCache[string, *ModelService](capacity, ttl).WithMetrics("model_services", metrics),
	}
}

func (r *CachedModelServiceReader) GetByModel(ctx context.Context, model string) (*ModelService, error) {
	return r.cache.GetOrLoad(ctx, model, func(ctx context.Context) (*ModelService, bool, error) {
		v, err := r.inner.GetByModel(ctx, model)
		return v, err == nil && v != nil, err
	})
}

// List 不缓存——调用方一般在启动期 / 巡检用，命中频次低。
func (r *CachedModelServiceReader) List(ctx context.Context) ([]*ModelService, error) {
	return r.inner.List(ctx)
}

// =============================================================================
// CachedEndpointReader — wraps SQLEndpointReader with per-(model,group) TTL LRU
// =============================================================================

// CachedEndpointReader 缓存 ListForModel / GetByID / PickForModel 结果。
//
// **不缓存 List()**——调用方一般在启动期 / 巡检用。
type CachedEndpointReader struct {
	inner     *SQLEndpointReader
	listCache *TTLCache[string, []*Endpoint] // key = "model\x00group"
	idCache   *TTLCache[int64, *Endpoint]
}

// NewCachedEndpointReader 默认 listCapacity=1024（model×group 对数），
// idCapacity=4096（按 id 查），ttl=30s。
func NewCachedEndpointReader(inner *SQLEndpointReader, listCap, idCap int, ttl time.Duration, metrics Metrics) *CachedEndpointReader {
	return &CachedEndpointReader{
		inner:     inner,
		listCache: NewTTLCache[string, []*Endpoint](listCap, ttl).WithMetrics("endpoints_list", metrics),
		idCache:   NewTTLCache[int64, *Endpoint](idCap, ttl).WithMetrics("endpoints_id", metrics),
	}
}

func (r *CachedEndpointReader) ListForModel(ctx context.Context, model, group string) ([]*Endpoint, error) {
	if group == "" {
		group = "default"
	}
	key := model + "\x00" + group
	return r.listCache.GetOrLoad(ctx, key, func(ctx context.Context) ([]*Endpoint, bool, error) {
		v, err := r.inner.ListForModel(ctx, model, group)
		return v, err == nil && len(v) > 0, err
	})
}

// PickForModel 走 ListForModel 缓存的第一条。
func (r *CachedEndpointReader) PickForModel(ctx context.Context, model, group string) (*Endpoint, error) {
	list, err := r.ListForModel(ctx, model, group)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		// 跟 SQL 实现保持错误风格——not found 返 error。
		return r.inner.PickForModel(ctx, model, group)
	}
	return list[0], nil
}

func (r *CachedEndpointReader) GetByID(ctx context.Context, id int64) (*Endpoint, error) {
	return r.idCache.GetOrLoad(ctx, id, func(ctx context.Context) (*Endpoint, bool, error) {
		v, err := r.inner.GetByID(ctx, id)
		return v, err == nil && v != nil, err
	})
}

// List 不缓存——启动期 / health prober 用。
func (r *CachedEndpointReader) List(ctx context.Context) ([]*Endpoint, error) {
	return r.inner.List(ctx)
}

// 编译期断言：CachedEndpointReader 满足 EndpointReader 接口。
var _ EndpointReader = (*CachedEndpointReader)(nil)

// =============================================================================
// CachedQuotaPolicyProvider — wraps SQLQuotaPolicyProvider with per-id TTL LRU
// =============================================================================

// CachedQuotaPolicyProvider 缓存 GetByID(id) → *QuotaPolicy。
type CachedQuotaPolicyProvider struct {
	inner *SQLQuotaPolicyProvider
	cache *TTLCache[int64, *QuotaPolicy]
}

// NewCachedQuotaPolicyProvider 默认 capacity=128（少量 policy 共享）/ ttl=30s。
func NewCachedQuotaPolicyProvider(inner *SQLQuotaPolicyProvider, capacity int, ttl time.Duration, metrics Metrics) *CachedQuotaPolicyProvider {
	return &CachedQuotaPolicyProvider{
		inner: inner,
		cache: NewTTLCache[int64, *QuotaPolicy](capacity, ttl).WithMetrics("quota_policies", metrics),
	}
}

func (p *CachedQuotaPolicyProvider) GetByID(ctx context.Context, id int64) (*QuotaPolicy, error) {
	return p.cache.GetOrLoad(ctx, id, func(ctx context.Context) (*QuotaPolicy, bool, error) {
		v, err := p.inner.GetByID(ctx, id)
		return v, err == nil && v != nil, err
	})
}

// 编译期断言。
var _ QuotaPolicyProvider = (*CachedQuotaPolicyProvider)(nil)

// =============================================================================
// CachedSubscriptionProvider — wraps SQLSubscriptionProvider with per-pair TTL LRU
// =============================================================================

// CachedSubscriptionProvider 缓存 Has(accountID, modelServiceID) → bool。
//
// **缓存 false 值**——subscription 不存在跟"刚被删除"语义相同，TTL 内一致即可。
type CachedSubscriptionProvider struct {
	inner *SQLSubscriptionProvider
	cache *TTLCache[string, bool]
}

// NewCachedSubscriptionProvider 默认 capacity=10240（active subscriptions）/ ttl=30s。
func NewCachedSubscriptionProvider(inner *SQLSubscriptionProvider, capacity int, ttl time.Duration, metrics Metrics) *CachedSubscriptionProvider {
	return &CachedSubscriptionProvider{
		inner: inner,
		cache: NewTTLCache[string, bool](capacity, ttl).WithMetrics("subscriptions", metrics),
	}
}

// Has 跟其它 cached wrapper 不同：false 也缓存（loader 返 cache=true）。
func (p *CachedSubscriptionProvider) Has(ctx context.Context, accountID string, modelServiceID int64) (bool, error) {
	key := accountID + "\x00" + strconv.FormatInt(modelServiceID, 10)
	return p.cache.GetOrLoad(ctx, key, func(ctx context.Context) (bool, bool, error) {
		v, err := p.inner.Has(ctx, accountID, modelServiceID)
		return v, err == nil, err
	})
}
