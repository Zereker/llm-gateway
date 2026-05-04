package infra

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisConfig Redis 客户端连接配置（M6 RateLimit + 未来 cache layer 共享）。
//
// pkg/config 通过引用本类型把字段暴露给 yaml；用户写：
//
//	redis:
//	  addr: localhost:6379
//	  db: 0
//	  password: ""
//
// **注意**：v0.5 后 Redis 是 gateway 的 hard dependency（M6 限流必须）；
// 启动期 ping 失败 fail-fast。生产配主备 / sentinel；本结构体目前只支持单实例
// （go-redis Client；未来扩 ClusterClient 时加 driver 字段）。
type RedisConfig struct {
	Addr     string `yaml:"addr"`     // host:port
	DB       int    `yaml:"db"`       // 默认 0
	Password string `yaml:"password"` // 默认 ""
}

// OpenRedis 按 cfg 打开 *redis.Client 并 ping 验证。
//
// 应用层只在 main 调一次，整个进程共享一个 client（go-redis 内部连接池）。
// 调用方负责 defer client.Close()。
func OpenRedis(cfg RedisConfig) (*redis.Client, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("infra: redis addr required")
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		DB:       cfg.DB,
		Password: cfg.Password,
		// 连接池默认（go-redis 默认 10 × CPU）够用；上量再调
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("infra: redis ping %s: %w", cfg.Addr, err)
	}
	return rdb, nil
}
