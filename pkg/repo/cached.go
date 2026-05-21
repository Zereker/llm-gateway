package repo

import (
	"context"
	"time"
)

// 本文件给 repo 的 5 个 SQL Reader/Provider 各包一层 TTL 缓存——gateway 启动时
// cmd 拿 cached 版本注入到 middleware / dispatch port。repo 层唯一的缓存策略。
//
// **设计**：
//   - 每个 cached wrapper 嵌一个 sql reader + 一个 TTLCache
//   - cache key 是查询参数（Resolve(creds) 用 api_key_hash；GetByID(id) 用 id；
//     ListForModel(model,group) 用 "model:group" 复合 string；等等）
//   - "not found"（loader 返 nil 或没数据）**不**进缓存——让"刚创建的资源"立刻生效，
//     不被 negative cache 卡 TTL 时长
//   - 默认参数（capacity / ttl）给个 sensible default；cmd 可以按需调整
//
// **不做 stale-while-revalidate / refresh-ahead**——简单 TTL 过期即剔除；下次
// Get miss 再回源。如果需要异步刷新，留给将来 metric 显示 hit rate 低时再做。

// =============================================================================
// CachedAPIKeyProvider — wraps SQLAPIKeyProvider with per-(hash) TTL LRU
// =============================================================================

// CachedAPIKeyProvider 用 TTL LRU 缓存 Resolve 结果。cache key = api_key_hash。
//
// **不缓存 hash 计算**：SQLAPIKeyProvider.Resolve 内部已用 sha256；cache 只针对
// "hash → UserIdentity" 一对。
type CachedAPIKeyProvider struct {
	inner *SQLAPIKeyProvider
	cache *TTLCache[string, *UserIdentity]
}

// NewCachedAPIKeyProvider 构造。
//
// 默认 capacity=10240（支持几千个并发活跃 key）/ ttl=30s。
func NewCachedAPIKeyProvider(inner *SQLAPIKeyProvider, capacity int, ttl time.Duration) *CachedAPIKeyProvider {
	return &CachedAPIKeyProvider{
		inner: inner,
		cache: NewTTLCache[string, *UserIdentity](capacity, ttl),
	}
}

// Resolve 实现 APIKeyProvider.Resolve；cache hit 短路；miss 走 SQL + 回填 cache。
//
// 缓存 key = SHA-256(plaintext)——SQLAPIKeyProvider.Resolve 内部 hash；这里
// 我们也 hash 一遍以便 cache 命中前不接触 SQL。同 plaintext 两次调用第二次命中。
func (p *CachedAPIKeyProvider) Resolve(ctx context.Context, creds *Credentials) (*UserIdentity, error) {
	if creds == nil || creds.APIKey == "" {
		return p.inner.Resolve(ctx, creds)
	}
	key := HashAPIKey(creds.APIKey)
	if v, ok := p.cache.Get(key); ok {
		return v, nil
	}
	v, err := p.inner.Resolve(ctx, creds)
	if err != nil {
		return nil, err
	}
	if v != nil {
		p.cache.Set(key, v)
	}
	return v, nil
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
func NewCachedModelServiceReader(inner *SQLModelServiceReader, capacity int, ttl time.Duration) *CachedModelServiceReader {
	return &CachedModelServiceReader{
		inner: inner,
		cache: NewTTLCache[string, *ModelService](capacity, ttl),
	}
}

// GetByModel 实现 ModelServiceReader.GetByModel；cache hit 短路。
func (r *CachedModelServiceReader) GetByModel(ctx context.Context, model string) (*ModelService, error) {
	if v, ok := r.cache.Get(model); ok {
		return v, nil
	}
	v, err := r.inner.GetByModel(ctx, model)
	if err != nil {
		return nil, err
	}
	if v != nil {
		r.cache.Set(model, v)
	}
	return v, nil
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
func NewCachedEndpointReader(inner *SQLEndpointReader, listCap, idCap int, ttl time.Duration) *CachedEndpointReader {
	return &CachedEndpointReader{
		inner:     inner,
		listCache: NewTTLCache[string, []*Endpoint](listCap, ttl),
		idCache:   NewTTLCache[int64, *Endpoint](idCap, ttl),
	}
}

// ListForModel 实现 EndpointReader.ListForModel；缓存 (model, group) → []*Endpoint。
func (r *CachedEndpointReader) ListForModel(ctx context.Context, model, group string) ([]*Endpoint, error) {
	if group == "" {
		group = "default"
	}
	key := model + "\x00" + group
	if v, ok := r.listCache.Get(key); ok {
		return v, nil
	}
	v, err := r.inner.ListForModel(ctx, model, group)
	if err != nil {
		return nil, err
	}
	if len(v) > 0 {
		r.listCache.Set(key, v)
	}
	return v, nil
}

// PickForModel 实现 EndpointReader.PickForModel；走 ListForModel 缓存的第一条。
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

// GetByID 实现 EndpointReader.GetByID；缓存 id → *Endpoint。
func (r *CachedEndpointReader) GetByID(ctx context.Context, id int64) (*Endpoint, error) {
	if v, ok := r.idCache.Get(id); ok {
		return v, nil
	}
	v, err := r.inner.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if v != nil {
		r.idCache.Set(id, v)
	}
	return v, nil
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
func NewCachedQuotaPolicyProvider(inner *SQLQuotaPolicyProvider, capacity int, ttl time.Duration) *CachedQuotaPolicyProvider {
	return &CachedQuotaPolicyProvider{
		inner: inner,
		cache: NewTTLCache[int64, *QuotaPolicy](capacity, ttl),
	}
}

// GetByID 实现 QuotaPolicyProvider.GetByID；cache hit 短路。
func (p *CachedQuotaPolicyProvider) GetByID(ctx context.Context, id int64) (*QuotaPolicy, error) {
	if v, ok := p.cache.Get(id); ok {
		return v, nil
	}
	v, err := p.inner.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if v != nil {
		p.cache.Set(id, v)
	}
	return v, nil
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

type subsCacheValue bool

// NewCachedSubscriptionProvider 默认 capacity=10240（active subscriptions）/ ttl=30s。
func NewCachedSubscriptionProvider(inner *SQLSubscriptionProvider, capacity int, ttl time.Duration) *CachedSubscriptionProvider {
	return &CachedSubscriptionProvider{
		inner: inner,
		cache: NewTTLCache[string, bool](capacity, ttl),
	}
}

// Has 实现 SubscriptionProvider.Has；缓存 (accountID, modelServiceID) → bool。
//
// 跟其它 cached wrapper 不同：这里 false 也缓存（订阅不存在跟存在一样需要快速判定）。
func (p *CachedSubscriptionProvider) Has(ctx context.Context, accountID string, modelServiceID int64) (bool, error) {
	key := accountID + "\x00" + itoa(modelServiceID)
	if v, ok := p.cache.Get(key); ok {
		return v, nil
	}
	v, err := p.inner.Has(ctx, accountID, modelServiceID)
	if err != nil {
		return false, err
	}
	p.cache.Set(key, v)
	return v, nil
}

// itoa 避免 strconv 在 hot path 上额外 import；够小的 int64 用串拼接。
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
