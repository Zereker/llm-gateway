package infra

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisConfig is the Redis client connection configuration (shared by M6
// RateLimit and a future cache layer).
//
// pkg/config exposes these fields to yaml by referencing this type; the
// user writes:
//
//	redis:
//	  addr: localhost:6379
//	  db: 0
//	  password: ""
//
// **Note**: as of v0.5 Redis is a hard dependency of the gateway (required
// by M6 rate limiting); a ping failure at startup is fail-fast. Production
// should configure primary/standby or sentinel; this struct currently only
// supports a single instance (go-redis Client; a driver field will be
// added when ClusterClient support is extended in the future).
type RedisConfig struct {
	Addr     string `yaml:"addr"`     // host:port
	DB       int    `yaml:"db"`       // default 0
	Password string `yaml:"password"` // default ""
}

// OpenRedis opens a *redis.Client per cfg and verifies it with a ping.
//
// The application layer calls this once in main; the whole process
// shares one client (go-redis's internal connection pool). The caller is
// responsible for deferring client.Close().
func OpenRedis(cfg RedisConfig) (*redis.Client, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("infra: redis addr required")
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		DB:       cfg.DB,
		Password: cfg.Password,
		// Connection pool defaults (go-redis defaults to 10 x CPU) are
		// sufficient; tune when load increases
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("infra: redis ping %s: %w", cfg.Addr, err)
	}
	return rdb, nil
}
