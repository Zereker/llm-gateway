package repo

import (
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
)

// TTLCache 是带 TTL 的 LRU——容量满淘汰最近最少用，TTL 过期 Get 视为未命中。
// 实现就是 hashicorp/golang-lru v2 expirable.LRU 的薄包装，统一项目内 cache 入口。
//
// **使用模式**：repo 层 cached wrapper 拿一个 TTLCache 包一层 SQL Reader。
//
//	c := repo.NewTTLCache[int64, *Endpoint](1024, 30*time.Second)
//	if v, ok := c.Get(id); ok { return v, nil }
//	v, err := sqlReader.GetByID(ctx, id)
//	if err == nil { c.Set(id, v) }
//
// **设计权衡**：repo 端用 TTL 缓存替代实时失效。deployer SQL 写完后 ≤ TTL 才能
// 保证 gateway 看到新值；接受这个延迟，因为业务表变更（endpoint / api_key 启用 /
// quota 调整）不需要秒级。
//
// **不缓存"不存在"**：loader 返回 nil/zero 时不 Set，避免雪崩缓存 not-found
// 让"刚创建的资源"立即生效。
type TTLCache[K comparable, V any] struct {
	inner *lru.LRU[K, V]
}

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
