package repo

import (
	"context"
	"fmt"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
)

// TTLCache 是带 TTL 的 LRU + singleflight loader——容量满淘汰最近最少用，
// TTL 过期 Get 视为未命中；GetOrLoad 把"miss → 调 loader → 回填"收成一步，
// 同 key 并发 miss 只调 loader 一次（防雪崩）。
//
// 底层是 hashicorp/golang-lru v2 expirable.LRU 的薄包装。
//
// **使用模式**：repo 层 cached wrapper 用 GetOrLoad 包 SQL Reader。
//
//	c := repo.NewTTLCache[int64, *Endpoint](1024, 30*time.Second)
//	ep, err := c.GetOrLoad(ctx, id, func(ctx context.Context) (*Endpoint, bool, error) {
//	    v, err := sqlReader.GetByID(ctx, id)
//	    if err != nil { return nil, false, err }
//	    return v, v != nil, nil   // not-found（v==nil）不缓存
//	})
//
// **设计权衡**：repo 端用 TTL 缓存替代实时失效。deployer SQL 写完后 ≤ TTL 才能
// 保证 gateway 看到新值；接受这个延迟，因为业务表变更（endpoint / api_key 启用 /
// quota 调整）不需要秒级。
type TTLCache[K comparable, V any] struct {
	inner *lru.LRU[K, V]
	sf    singleflight.Group
}

// LoaderFunc 是 GetOrLoad 的 miss 回调。
//
// 返回 (value, cache, err)：
//   - err != nil：不缓存，错误透传给 caller
//   - err == nil && cache == true：value 回填 cache 并返回
//   - err == nil && cache == false：value 直接返回，不进 cache
//     （用于 not-found：避免缓存"刚创建的资源"还没生效）
type LoaderFunc[V any] func(ctx context.Context) (V, bool, error)

// NewTTLCache 构造一个 TTLCache。capacity<=0 时 fallback 到 1024。
func NewTTLCache[K comparable, V any](capacity int, ttl time.Duration) *TTLCache[K, V] {
	if capacity <= 0 {
		capacity = 1024
	}
	return &TTLCache[K, V]{inner: lru.NewLRU[K, V](capacity, nil, ttl)}
}

func (c *TTLCache[K, V]) Get(key K) (V, bool) { return c.inner.Get(key) }
func (c *TTLCache[K, V]) Set(key K, val V)    { c.inner.Add(key, val) }
func (c *TTLCache[K, V]) Delete(key K)        { c.inner.Remove(key) }
func (c *TTLCache[K, V]) Len() int            { return c.inner.Len() }

// GetOrLoad cache hit 直接返回；miss 时 singleflight 调 loader 加载。
//
// 同 key 并发 miss 只会调用 loader 一次，其他调用阻塞等同一结果；
// loader panic / err 透传给所有阻塞者。
func (c *TTLCache[K, V]) GetOrLoad(ctx context.Context, key K, loader LoaderFunc[V]) (V, error) {
	if v, ok := c.inner.Get(key); ok {
		return v, nil
	}
	// singleflight key 是 string；comparable 用 fmt.Sprintf 即可（miss path 不是 hot path）
	sfKey := fmt.Sprintf("%v", key)
	raw, err, _ := c.sf.Do(sfKey, func() (any, error) {
		// 等阻塞结束后可能已经有别的 goroutine 回填了，再查一次
		if v, ok := c.inner.Get(key); ok {
			return v, nil
		}
		v, cache, err := loader(ctx)
		if err != nil {
			return v, err
		}
		if cache {
			c.inner.Add(key, v)
		}
		return v, nil
	})
	if err != nil {
		var zero V
		return zero, err
	}
	return raw.(V), nil
}
