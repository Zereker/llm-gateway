package repo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/zereker/llm-gateway/internal/domain"
)

// This file wraps each of repo's 5 SQL Readers/Providers in a TTL cache — at
// gateway startup cmd injects the cached versions into middleware / dispatch
// ports. This is the repo layer's only caching strategy.
//
// **Design**:
//   - Each cached wrapper embeds a sql reader + a TTLCache
//   - Every hot path goes through TTLCache.GetOrLoad (miss + singleflight in one step)
//   - The cache key is the query parameter (Resolve(creds) uses api_key_hash;
//     GetByID(id) uses id; ListForModel(model,group) uses the composite string
//     "model\x00group"; etc.)
//   - "not found" (loader returns nil or no data) is **not** cached — the
//     loader tells the cache layer explicitly by returning cache=false. This
//     lets a "just-created resource" take effect immediately, instead of
//     being stuck behind a negative-cache TTL
//   - Default parameters (capacity / ttl) get a sensible default; cmd can tune
//     them as needed

// =============================================================================
// CachedAPIKeyProvider — wraps SQLAPIKeyProvider with per-(hash) TTL LRU
// =============================================================================

// negativeTTL is the negative-cache duration for invalid credentials / false subscriptions.
//
// **Why a negative cache**: M2 Auth runs before M6 RateLimit — without a
// negative cache, hammering a non-existent key floods traffic straight into
// MySQL 1:1 (an unauthenticated DoS amplifier).
//
// **Why short**: the cost of a negative cache is that a "just-created /
// just-subscribed resource" can take up to negativeTTL to become visible. 5s
// squeezes credential-stuffing traffic down from per-request to
// per-5s-per-key, while the deployer's "INSERT then curl to verify" workflow
// sees negligible added latency.
const negativeTTL = 5 * time.Second

// CachedAPIKeyProvider caches Resolve results with a TTL LRU. cache key = api_key_hash.
//
// Dual cache: positive (hash -> identity, 30s) + negative (hash -> invalid, 5s).
// **Only ErrInvalidCredentials goes into the negative cache** — DB failures
// are not cached (retried next time).
type CachedAPIKeyProvider struct {
	inner    *SQLAPIKeyProvider
	cache    *TTLCache[string, *UserIdentity]
	negative *TTLCache[string, struct{}]
}

// NewCachedAPIKeyProvider defaults to capacity=10240 (supports several
// thousand concurrently active keys) / ttl=30s. No reporting when metrics is nil.
func NewCachedAPIKeyProvider(inner *SQLAPIKeyProvider, capacity int, ttl time.Duration, metrics Metrics) *CachedAPIKeyProvider {
	return &CachedAPIKeyProvider{
		inner:    inner,
		cache:    NewTTLCache[string, *UserIdentity](capacity, ttl).WithMetrics("api_keys", metrics),
		negative: NewTTLCache[string, struct{}](capacity, negativeTTL).WithMetrics("api_keys_negative", metrics),
	}
}

func (p *CachedAPIKeyProvider) Resolve(ctx context.Context, creds *Credentials) (*UserIdentity, error) {
	if creds == nil || creds.APIKey == "" {
		return p.inner.Resolve(ctx, creds)
	}
	key := HashAPIKey(creds.APIKey)
	if _, invalid := p.negative.Get(key); invalid {
		return nil, fmt.Errorf("apikey: %w", domain.ErrInvalidCredentials)
	}
	v, err := p.cache.GetOrLoad(ctx, key, func(ctx context.Context) (*UserIdentity, bool, error) {
		u, err := p.inner.Resolve(ctx, creds)
		return u, err == nil && u != nil, err
	})
	if err != nil && errors.Is(err, domain.ErrInvalidCredentials) {
		p.negative.Set(key, struct{}{})
	}
	return v, err
}

// Evict removes both the positive and negative cache entries for an
// api_key_hash. The control plane calls this via cachebus notification when
// revoking a key, shrinking the "still cached as valid after revocation"
// window from <=TTL down to sub-second.
//
// hash is just HashAPIKey(plaintext) (= the DB api_key_hash column) — the
// control plane holds it, so the data plane never needs the plaintext.
// Clearing the negative cache too is for symmetry: in the rare race where a
// hash happens to be negatively cached, that gets wiped as well.
func (p *CachedAPIKeyProvider) Evict(hash string) {
	p.cache.Delete(hash)
	p.negative.Delete(hash)
}

// =============================================================================
// CachedModelServiceReader — wraps SQLModelServiceReader with per-model TTL LRU
// =============================================================================

// CachedModelServiceReader caches GetByModel(model) results.
type CachedModelServiceReader struct {
	inner *SQLModelServiceReader
	cache *TTLCache[string, *ModelService]
}

// NewCachedModelServiceReader defaults to capacity=256 / ttl=30s.
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

// List is not cached — callers typically use it at startup / for health
// checks, where hit frequency is low.
func (r *CachedModelServiceReader) List(ctx context.Context) ([]*ModelService, error) {
	return r.inner.List(ctx)
}

// =============================================================================
// CachedEndpointReader — wraps SQLEndpointReader with per-(model,group) TTL LRU
// =============================================================================

// CachedEndpointReader caches ListForModel / GetByID / PickForModel results.
//
// **List() is not cached** — callers typically use it at startup / for health checks.
type CachedEndpointReader struct {
	inner     *SQLEndpointReader
	listCache *TTLCache[string, []*Endpoint] // key = "model\x00group"
	idCache   *TTLCache[int64, *Endpoint]
}

// NewCachedEndpointReader defaults to listCapacity=1024 (number of
// model x group pairs), idCapacity=4096 (lookup by id), ttl=30s.
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

// PickForModel takes the first entry from ListForModel's cache.
func (r *CachedEndpointReader) PickForModel(ctx context.Context, model, group string) (*Endpoint, error) {
	list, err := r.ListForModel(ctx, model, group)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		// Keep the error style consistent with the SQL implementation — not
		// found returns an error.
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

// List is not cached — used at startup / by the health prober.
func (r *CachedEndpointReader) List(ctx context.Context) ([]*Endpoint, error) {
	return r.inner.List(ctx)
}

// Compile-time assertion: CachedEndpointReader satisfies the EndpointReader interface.
var _ EndpointReader = (*CachedEndpointReader)(nil)

// =============================================================================
// CachedQuotaPolicyProvider — wraps SQLQuotaPolicyProvider with per-id TTL LRU
// =============================================================================

// CachedQuotaPolicyProvider caches GetByID(id) -> *QuotaPolicy.
type CachedQuotaPolicyProvider struct {
	inner *SQLQuotaPolicyProvider
	cache *TTLCache[int64, *QuotaPolicy]
}

// NewCachedQuotaPolicyProvider defaults to capacity=128 (a small number of shared policies) / ttl=30s.
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

// Compile-time assertion.
var _ QuotaPolicyProvider = (*CachedQuotaPolicyProvider)(nil)

// =============================================================================
// CachedSubscriptionProvider — wraps SQLSubscriptionProvider with per-pair TTL LRU
// =============================================================================

// CachedSubscriptionProvider caches Has(accountID, modelServiceID) -> bool.
//
// **true / false use different TTLs**:
//   - true  -- 30s (the normal TTL; a subscription cancellation taking up to
//     30s to take effect is acceptable)
//   - false -- negativeTTL 5s (short): so the deployer's "INSERT subscription
//     then curl to verify" workflow doesn't hit the 30s 403 window, and the
//     time a multi-replica LB spends flip-flopping between 200/403 is also
//     squeezed down to seconds
type CachedSubscriptionProvider struct {
	inner      *SQLSubscriptionProvider
	trueCache  *TTLCache[string, struct{}]
	falseCache *TTLCache[string, struct{}]
}

// NewCachedSubscriptionProvider defaults to capacity=10240 (active
// subscriptions) / ttl=30s (for the true side).
func NewCachedSubscriptionProvider(inner *SQLSubscriptionProvider, capacity int, ttl time.Duration, metrics Metrics) *CachedSubscriptionProvider {
	return &CachedSubscriptionProvider{
		inner:      inner,
		trueCache:  NewTTLCache[string, struct{}](capacity, ttl).WithMetrics("subscriptions", metrics),
		falseCache: NewTTLCache[string, struct{}](capacity, negativeTTL).WithMetrics("subscriptions_negative", metrics),
	}
}

// Has caches true / false separately in caches with different TTLs; DB errors are not cached.
func (p *CachedSubscriptionProvider) Has(ctx context.Context, accountID string, modelServiceID int64) (bool, error) {
	key := accountID + "\x00" + strconv.FormatInt(modelServiceID, 10)
	if _, ok := p.trueCache.Get(key); ok {
		return true, nil
	}
	if _, ok := p.falseCache.Get(key); ok {
		return false, nil
	}
	v, err := p.inner.Has(ctx, accountID, modelServiceID)
	if err != nil {
		return false, err
	}
	if v {
		p.trueCache.Set(key, struct{}{})
	} else {
		p.falseCache.Set(key, struct{}{})
	}
	return v, nil
}
