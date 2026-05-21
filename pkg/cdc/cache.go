package cdc

import (
	"context"
	"encoding/json"
	"sync"
)

// TieredCache 三层读取缓存（L1 进程内 LRU + L2 Redis 缓存键 + L3 loader fallback）。
//
// **使用模式**（gateway 端 ModelCatalog 装配）：
//
//	cache := cdc.NewTieredCache[*domain.ModelService](cdc.TieredConfig{
//	    Table:  "model_services",
//	    Local:  cdc.NewLRU[*domain.ModelService](1024),
//	    Loader: func(ctx, pk) (*domain.ModelService, error) { return sqlReader.GetByModel(ctx, pk) },
//	})
//	// CDC consumer 接 cache.HandleEvent 做 invalidate（按 Debezium event）
//
// **数据流**：
//   - Get(pk) → L1 命中? → 否 → L3 loader (MySQL) → 写 L1 → 返回
//   - CDC event 到达 → HandleEvent → 解析出 PK → L1.Delete(pk) → 下次请求重新 load
//
// 注意：本设计**简化为 L1 + L3**（去掉 L2 Redis 主动 SET）——理由：
//   - Debezium event 直接驱动 L1 失效；不需要 Redis 当二级缓存
//   - 多副本 gateway 各自 XREAD 同一 stream（fan-out），各自维护 L1
//   - 减少一次 Redis Get hop（每请求 1ms 左右）
type TieredCache[T any] struct {
	cfg     TieredConfig
	loader  func(ctx context.Context, pk string) (T, error)
	pkOf    func(T) string // 实体 → PK 字符串（用于 invalidate after CDC event）
	local   LocalCache[T]
	mu      sync.RWMutex
}

// TieredConfig 装配参数。
type TieredConfig struct {
	Table string // 必填；与 Debezium event source.table 匹配
}

// LocalCache L1 接口。
type LocalCache[T any] interface {
	Get(key string) (val T, ok bool)
	Set(key string, val T)
	Delete(key string)
	Clear()
}

// NewTieredCache 构造。
//
// pkOf：把实体反向得到 PK string（CDC event 解析出 row 后用于 cache key）。
// loader：L3 fallback；通常包 repo.SQLXxxReader.GetByXxx。
func NewTieredCache[T any](
	cfg TieredConfig,
	local LocalCache[T],
	pkOf func(T) string,
	loader func(ctx context.Context, pk string) (T, error),
) *TieredCache[T] {
	return &TieredCache[T]{
		cfg:    cfg,
		loader: loader,
		pkOf:   pkOf,
		local:  local,
	}
}

// Get 按 PK 取实体（L1 命中 / L3 loader）。
//
// 返回 (zero, nil) = L3 也找不到（业务层视作 not-found，不缓存）。
func (c *TieredCache[T]) Get(ctx context.Context, pk string) (T, error) {
	var zero T

	c.mu.RLock()
	if v, ok := c.local.Get(pk); ok {
		c.mu.RUnlock()
		return v, nil
	}
	c.mu.RUnlock()

	if c.loader == nil {
		return zero, nil
	}
	v, err := c.loader(ctx, pk)
	if err != nil {
		return zero, err
	}
	if isZero(v) {
		// not found：不缓存（避免缓存"不存在"；下次请求或许 deployer 已创建）
		return zero, nil
	}

	c.mu.Lock()
	c.local.Set(pk, v)
	c.mu.Unlock()
	return v, nil
}

// HandleEvent CDC consumer 调本方法把 Debezium event 转成 L1 失效操作。
//
// 接的是 StreamConsumer 的 EventHandler；只关心 c.cfg.Table 的事件。
func (c *TieredCache[T]) HandleEvent(ctx context.Context, table string, e *Event) error {
	if table != c.cfg.Table {
		return nil
	}
	row := e.PrimaryRow()
	if len(row) == 0 {
		return nil
	}
	// 反序列化拿 PK
	var v T
	if err := json.Unmarshal(row, &v); err != nil {
		// 解析失败：保守 Clear()，强制下次重新 load 所有
		c.mu.Lock()
		c.local.Clear()
		c.mu.Unlock()
		return nil
	}
	pk := c.pkOf(v)
	if pk == "" {
		return nil
	}
	c.mu.Lock()
	c.local.Delete(pk)
	c.mu.Unlock()
	return nil
}

// =============================================================================
// LRU 实现
// =============================================================================

// lruCache sync 简单 LRU；够 hot-path 用，不追求严格 LRU。
type lruCache[T any] struct {
	mu    sync.Mutex
	data  map[string]T
	order []string
	cap   int
}

// NewLRU 固定容量本地缓存。capacity <= 0 用 1024。
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

func (c *lruCache[T]) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = make(map[string]T, c.cap)
	c.order = c.order[:0]
}

// isZero 判断零值（json null）。
func isZero[T any](v T) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return string(b) == "null"
}
