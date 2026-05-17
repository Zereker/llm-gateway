package cdcoutbox

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// TieredCache 三层读取缓存（L1 进程内 + L2 Redis + L3 loader fallback）。
//
// **使用模式**（gateway 端）：
//
//	cache := cdcoutbox.NewTieredCache[*domain.ModelService](cdcoutbox.TieredConfig{
//	    Table:     "model_services",
//	    Prefix:    "llm:cache",
//	    Redis:     rdb,
//	    Local:     cdcoutbox.NewLRU(1024),
//	    LocalTTL:  10 * time.Minute,
//	    Loader:    func(ctx, pk) (*domain.ModelService, error) { return sqlReader.GetByModel(ctx, pk) },
//	})
//	cache.SubscribeInvalidations(ctx, "llm-gateway.invalidate")
//	ms, _ := cache.Get(ctx, "gpt-4o")
//
// **数据流**：
//   - Get(pk) → 查 L1 → 命中返回 ; 否则查 L2 Redis → 命中 unmarshal + 写 L1 ; 否则 L3 loader → 写 L2 + L1
//   - PUBSUB invalidation 来一条 → L1 Delete (L2 由 admin relay 已更新)
//
// 业务结构 T 必须能 json.Marshal/Unmarshal。
type TieredCache[T any] struct {
	cfg     TieredConfig
	loader  func(ctx context.Context, pk string) (T, error)
	local   LocalCache[T]
	rdb     *redis.Client
	keyFunc func(pk string) string

	mu      sync.RWMutex
	stopCh  chan struct{}
	stopped bool
}

// TieredConfig 装配参数。
type TieredConfig struct {
	Table  string // 必填；与 cdcoutbox.Change.Table 对应
	Prefix string // Redis key 前缀；默认 "llm:cache"

	Redis *redis.Client // L2；nil = 跳过 L2 直接走 loader

	LocalTTL time.Duration // L1 LRU 条目 TTL；0 = 不主动过期（靠 invalidation 通知刷新）
}

// LocalCache L1 接口（默认 NewLRU 实现）。
type LocalCache[T any] interface {
	Get(key string) (val T, ok bool)
	Set(key string, val T)
	Delete(key string)
}

// NewTieredCache 构造一个三层缓存。
//
// loader 是 L3（cold fallback），通常包装 repo.SQLXxxReader。
func NewTieredCache[T any](cfg TieredConfig, local LocalCache[T], loader func(ctx context.Context, pk string) (T, error)) *TieredCache[T] {
	if cfg.Prefix == "" {
		cfg.Prefix = "llm:cache"
	}
	c := &TieredCache[T]{
		cfg:     cfg,
		loader:  loader,
		local:   local,
		rdb:     cfg.Redis,
		stopCh:  make(chan struct{}),
		keyFunc: func(pk string) string { return cfg.Prefix + ":" + cfg.Table + ":" + pk },
	}
	return c
}

// Get 按 pk 取实体（L1 → L2 → L3）。
//
// 返回 (zero, nil) = L3 也找不到（业务层视作 not found）；
// 返回 (zero, err) = L2/L3 出 err。
func (c *TieredCache[T]) Get(ctx context.Context, pk string) (T, error) {
	var zero T

	// L1
	if v, ok := c.local.Get(pk); ok {
		return v, nil
	}

	// L2
	if c.rdb != nil {
		key := c.keyFunc(pk)
		raw, err := c.rdb.Get(ctx, key).Bytes()
		if err == nil {
			var v T
			if uerr := json.Unmarshal(raw, &v); uerr == nil {
				c.local.Set(pk, v)
				return v, nil
			}
			// 反序列化失败：fallthrough 到 loader（不应发生）
		}
		// redis.Nil 或其他 err → fallthrough 到 L3
	}

	// L3：loader（MySQL 兜底）
	if c.loader == nil {
		return zero, nil
	}
	v, err := c.loader(ctx, pk)
	if err != nil {
		return zero, err
	}
	// 若 loader 返回了空值（如 nil 指针），不写缓存（避免缓存"不存在"）
	if isZero(v) {
		return zero, nil
	}

	// 写回 L2（admin relay 同步 → 但 cold start 时可能没写过）
	if c.rdb != nil {
		if payload, mErr := json.Marshal(v); mErr == nil {
			_ = c.rdb.Set(ctx, c.keyFunc(pk), payload, 0).Err() // 0 = 不过期（由 relay 维护）
		}
	}
	c.local.Set(pk, v)
	return v, nil
}

// Invalidate 显式让 L1 失效（PUBSUB 消息消费时调）。
//
// **不**主动删 L2 Redis key——L2 是 admin relay 维护的真相。
func (c *TieredCache[T]) Invalidate(pk string) {
	c.local.Delete(pk)
}

// SubscribeInvalidations 启动后台 goroutine 监听 Redis PUBSUB；
// 收到属于本 cache.Table 的消息 → Invalidate(pk)。
//
// **必须** caller 在使用 Get 前调一次；返回 nil 表示已启动。
func (c *TieredCache[T]) SubscribeInvalidations(ctx context.Context, channel string) error {
	if c.rdb == nil {
		return nil
	}
	pubsub := c.rdb.Subscribe(ctx, channel)
	go func() {
		defer func() { _ = pubsub.Close() }()
		ch := pubsub.Channel()
		for {
			select {
			case <-c.stopCh:
				return
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				var inv InvalidationMessage
				if err := json.Unmarshal([]byte(msg.Payload), &inv); err != nil {
					continue
				}
				if inv.Table != c.cfg.Table {
					continue // 不是我关心的表
				}
				c.Invalidate(inv.PK)
			}
		}
	}()
	return nil
}

// Stop 终止后台 PUBSUB 订阅。
func (c *TieredCache[T]) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	c.stopped = true
	close(c.stopCh)
}

// isZero 判断泛型值是否零值（用于不缓存 nil 指针 / 空 struct）。
// 简化实现：用 json.Marshal 检查 == "null"；适用于 *T 指针场景。
func isZero[T any](v T) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return string(b) == "null"
}

// =============================================================================
// LRU L1 实现
// =============================================================================

// lruCache 简单的 sync.Map + ring-buffer LRU；够 hot-path 用。
//
// 跟 hashicorp/golang-lru 的本质区别：本实现追求零外部依赖；空间维度
// 通过粗粒度容量上限 + 周期清理控制，不追求严格 LRU。
type lruCache[T any] struct {
	mu    sync.Mutex
	data  map[string]T
	order []string
	cap   int
}

// NewLRU 构造一个固定容量的本地缓存。capacity <= 0 用 1024 默认。
func NewLRU[T any](capacity int) LocalCache[T] {
	if capacity <= 0 {
		capacity = 1024
	}
	return &lruCache[T]{
		data:  make(map[string]T, capacity),
		order: make([]string, 0, capacity),
		cap:   capacity,
	}
}

func (c *lruCache[T]) Get(key string) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[key]
	return v, ok
}

func (c *lruCache[T]) Set(key string, val T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.data[key]; !exists {
		if len(c.order) >= c.cap {
			// 淘汰最老
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.data, oldest)
		}
		c.order = append(c.order, key)
	}
	c.data[key] = val
}

func (c *lruCache[T]) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.data[key]; !ok {
		return
	}
	delete(c.data, key)
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}
