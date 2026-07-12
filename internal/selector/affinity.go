package selector

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// AffinityStore stores the session → endpoint mapping for session affinity (sticky routing).
//
// Purpose: pin the same session to the same upstream endpoint, improving vLLM prefix cache / KV cache
// hit rate (the endpoint's PrefixCacheEnabled capability bit signals this scenario).
//
// **Soft affinity**: just a "preference", not a hard binding — when the pinned endpoint gets
// cooldown'd / excluded / taken offline, the scheduler automatically reselects and re-pins
// (see scheduler.Pick). Needs to be consistent across replicas, hence Redis; nil = affinity disabled.
type AffinityStore interface {
	// Get retrieves the endpoint id currently pinned for a session; no mapping returns (0,false).
	Get(ctx context.Context, sessionKey string) (int64, bool)
	// Set records/refreshes the session → endpoint mapping (TTL is refreshed on every selection, keeping active sessions sticky).
	Set(ctx context.Context, sessionKey string, endpointID int64)
}

// RedisAffinityStore is a Redis implementation, shared across replicas.
type RedisAffinityStore struct {
	rdb    *redis.Client
	prefix string
	ttl    time.Duration
}

// NewRedisAffinityStore constructs one; empty prefix uses "llm-gateway:sched", ttl<=0 uses 10m.
func NewRedisAffinityStore(rdb *redis.Client, prefix string, ttl time.Duration) *RedisAffinityStore {
	if prefix == "" {
		prefix = "llm-gateway:sched"
	}

	if ttl <= 0 {
		ttl = 10 * time.Minute
	}

	return &RedisAffinityStore{rdb: rdb, prefix: prefix, ttl: ttl}
}

func (s *RedisAffinityStore) key(sessionKey string) string {
	return s.prefix + ":affinity:" + sessionKey
}

// Get reads the pin; missing / Redis error are both treated as no mapping (best-effort, does not block scheduling).
func (s *RedisAffinityStore) Get(ctx context.Context, sessionKey string) (int64, bool) {
	v, err := s.rdb.Get(ctx, s.key(sessionKey)).Result()
	if err != nil {
		return 0, false
	}

	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}

	return id, true
}

// Set writes the pin + refreshes TTL (best-effort).
func (s *RedisAffinityStore) Set(ctx context.Context, sessionKey string, endpointID int64) {
	if endpointID == 0 {
		return
	}

	_ = s.rdb.Set(ctx, s.key(sessionKey), endpointID, s.ttl).Err()
}

// Compile-time assertion.
var _ AffinityStore = (*RedisAffinityStore)(nil)
