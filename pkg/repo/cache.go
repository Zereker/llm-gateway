package repo

import (
	"container/list"
	"sync"
	"time"
)

// TTLCache 是带 TTL 的 LRU——容量满淘汰最近最少用，TTL 过期 Get 视为未命中。
//
// **使用模式**：repo 层 cached wrapper 拿一个 TTLCache 包一层 SQL Reader。
//
//	c := repo.NewTTLCache[int64, *Endpoint](1024, 30*time.Second)
//	if v, ok := c.Get(id); ok { return v, nil }
//	v, err := sqlReader.GetByID(ctx, id)
//	if err == nil { c.Set(id, v) }
//
// **并发安全**：单个 sync.Mutex 包整个表；hot path 是 Get + 1 个 map 查询 + 双向链表
// 移动，足够快。要更细粒度的话可以分片 lock，但当前数据量（万级别）一把锁够。
//
// **设计权衡**：repo 端用 TTL 缓存替代实时失效。deployer SQL 写完后 ≤ TTL 才能
// 保证 gateway 看到新值；接受这个延迟，因为业务表变更（endpoint / api_key 启用 /
// quota 调整）不需要秒级。
//
// **不缓存"不存在"**：loader 返回 nil/zero 时不 Set，避免雪崩缓存 not-found
// 让"刚创建的资源"立即生效。
type TTLCache[K comparable, V any] struct {
	mu       sync.Mutex
	items    map[K]*list.Element
	order    *list.List
	capacity int
	ttl      time.Duration
	now      func() time.Time // 注入便于测试
}

type ttlEntry[K comparable, V any] struct {
	key       K
	val       V
	expiresAt time.Time
}

// NewTTLCache 构造一个 TTLCache。
//
//	capacity ── 最大条目数；超出按 LRU 淘汰
//	ttl      ── 条目存活时长；过期 Get 视为未命中
func NewTTLCache[K comparable, V any](capacity int, ttl time.Duration) *TTLCache[K, V] {
	if capacity <= 0 {
		capacity = 1024
	}
	return &TTLCache[K, V]{
		items:    make(map[K]*list.Element, capacity),
		order:    list.New(),
		capacity: capacity,
		ttl:      ttl,
		now:      time.Now,
	}
}

// Get 取 key；未命中或已过期返回 (zero, false)。
func (c *TTLCache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	el, ok := c.items[key]
	if !ok {
		return zero, false
	}
	ent := el.Value.(*ttlEntry[K, V])
	if c.now().After(ent.expiresAt) {
		c.order.Remove(el)
		delete(c.items, key)
		return zero, false
	}
	c.order.MoveToFront(el)
	return ent.val, true
}

// Set 写入；已存在则刷新 TTL + LRU 顺序；超容量按 LRU 淘汰末尾。
func (c *TTLCache[K, V]) Set(key K, val V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	expiresAt := c.now().Add(c.ttl)
	if el, ok := c.items[key]; ok {
		ent := el.Value.(*ttlEntry[K, V])
		ent.val = val
		ent.expiresAt = expiresAt
		c.order.MoveToFront(el)
		return
	}
	ent := &ttlEntry[K, V]{key: key, val: val, expiresAt: expiresAt}
	el := c.order.PushFront(ent)
	c.items[key] = el
	if c.order.Len() > c.capacity {
		c.evictOldest()
	}
}

// Delete 主动失效——给手动重载用（生产正常路径靠 TTL 自动过期）。
func (c *TTLCache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.Remove(el)
		delete(c.items, key)
	}
}

// Len 当前条目数（含未过期但还没被 Get 触发清理的）。给 metric / 调试用。
func (c *TTLCache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

func (c *TTLCache[K, V]) evictOldest() {
	el := c.order.Back()
	if el == nil {
		return
	}
	ent := el.Value.(*ttlEntry[K, V])
	c.order.Remove(el)
	delete(c.items, ent.key)
}
