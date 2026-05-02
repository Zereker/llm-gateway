package store

import (
	"context"
	"time"
)

// Cache 限流 Lua 脚本 / Cooldown Manager / 配置二级缓存的存储后端抽象。
//
// 默认实现支持内存（单实例 / 测试）和 Redis（多实例共享）；
// 详见 docs/architecture/06-pluggable-infra.md。
type Cache interface {
	// 基础 KV
	Get(c context.Context, key string) ([]byte, error) // 不存在返回 nil, nil
	Set(c context.Context, key string, value []byte, ttl time.Duration) error
	Del(c context.Context, key string) error
	Exists(c context.Context, key string) (bool, error)

	// 原子计数
	Incr(c context.Context, key string, ttl time.Duration) (int64, error)
	IncrBy(c context.Context, key string, delta int64, ttl time.Duration) (int64, error)

	// 限流脚本（Lua / lua-style）
	EvalLimit(c context.Context, key string, capLimit, incr, ttlSec int64) (current int64, blocked bool, err error)
}
