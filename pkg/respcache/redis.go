// Package respcache is the Redis storage implementation of the response cache
// (middleware.ResponseCacheStore).
//
// It stores an entire non-streaming response (status/content-type/body/usage) as a
// single JSON blob, with a TTL. The body []byte is base64-encoded automatically by
// json.Marshal and decoded back on unmarshal. Best-effort: any read/write error is
// treated as a miss/no-op, and never blocks the request.
package respcache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore is a Redis-backed response cache, shared across multiple replicas.
type RedisStore struct {
	rdb    *redis.Client
	prefix string
}

// NewRedisStore constructs a RedisStore; if prefix is empty, "llm-gateway:respcache" is used.
func NewRedisStore(rdb *redis.Client, prefix string) *RedisStore {
	if prefix == "" {
		prefix = "llm-gateway:respcache"
	}
	return &RedisStore{rdb: rdb, prefix: prefix}
}

func (s *RedisStore) key(k string) string { return s.prefix + ":" + k }

// Get reads from the cache; a missing key, a Redis error, or a deserialization
// failure are all treated as a miss.
func (s *RedisStore) Get(ctx context.Context, key string) (CachedResponse, bool) {
	b, err := s.rdb.Get(ctx, s.key(key)).Bytes()
	if err != nil {
		return CachedResponse{}, false
	}
	var cr CachedResponse
	if json.Unmarshal(b, &cr) != nil {
		return CachedResponse{}, false
	}
	return cr, true
}

// Set writes to the cache with a TTL (best-effort).
func (s *RedisStore) Set(ctx context.Context, key string, resp CachedResponse, ttl time.Duration) {
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	_ = s.rdb.Set(ctx, s.key(key), b, ttl).Err()
}

// Compile-time assertion.
