// Package respcache 是响应缓存的 Redis 存储实现（middleware.ResponseCacheStore）。
//
// 存整条非流式响应(status/content-type/body/usage)为一个 JSON blob,带 TTL。
// body []byte 经 json.Marshal 自动 base64,读回 unmarshal。best-effort:读/写错都当
// miss/no-op,不阻塞请求。
package respcache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zereker/llm-gateway/pkg/middleware"
)

// RedisStore Redis-backed 响应缓存,多副本共享。
type RedisStore struct {
	rdb    *redis.Client
	prefix string
}

// NewRedisStore 构造;prefix 空用 "llm-gateway:respcache"。
func NewRedisStore(rdb *redis.Client, prefix string) *RedisStore {
	if prefix == "" {
		prefix = "llm-gateway:respcache"
	}
	return &RedisStore{rdb: rdb, prefix: prefix}
}

func (s *RedisStore) key(k string) string { return s.prefix + ":" + k }

// Get 读缓存;缺失 / Redis 错 / 反序列化失败都当 miss。
func (s *RedisStore) Get(ctx context.Context, key string) (middleware.CachedResponse, bool) {
	b, err := s.rdb.Get(ctx, s.key(key)).Bytes()
	if err != nil {
		return middleware.CachedResponse{}, false
	}
	var cr middleware.CachedResponse
	if json.Unmarshal(b, &cr) != nil {
		return middleware.CachedResponse{}, false
	}
	return cr, true
}

// Set 写缓存 + TTL(best-effort)。
func (s *RedisStore) Set(ctx context.Context, key string, resp middleware.CachedResponse, ttl time.Duration) {
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	_ = s.rdb.Set(ctx, s.key(key), b, ttl).Err()
}

// 编译期断言。
var _ middleware.ResponseCacheStore = (*RedisStore)(nil)
