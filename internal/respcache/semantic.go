package respcache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zereker/llm-gateway/internal/embed"
)

// RedisSemanticStore is the Redis implementation of the semantic cache (middleware.SemanticCacheStore).
//
// **Index shape**: each namespace gets one bounded LIST `<prefix>:sem:<ns>`, whose elements
// are {vec, resp} JSON. Lookup pulls the whole list and computes cosine similarity in Go
// (brute-force KNN) — entries per namespace are capped at maxEntries (via LTRIM), so at a
// scale of single digits to a few hundred, O(N) is negligible. To scale further, swap in a
// RediSearch vector index without changing the interface.
type RedisSemanticStore struct {
	rdb        *redis.Client
	prefix     string
	maxEntries int
}

// NewRedisSemanticStore constructs a RedisSemanticStore; an empty prefix uses the default,
// and maxEntries<=0 defaults to 500.
func NewRedisSemanticStore(rdb *redis.Client, prefix string, maxEntries int) *RedisSemanticStore {
	if prefix == "" {
		prefix = "llm-gateway:respcache"
	}

	if maxEntries <= 0 {
		maxEntries = 500
	}

	return &RedisSemanticStore{rdb: rdb, prefix: prefix, maxEntries: maxEntries}
}

func (s *RedisSemanticStore) key(ns string) string { return s.prefix + ":sem:" + ns }

type semanticEntry struct {
	Vec  []float32      `json:"vec"`
	Resp CachedResponse `json:"resp"`
}

// Lookup brute-force scans every entry in the namespace and returns the response with the
// highest cosine similarity that is ≥ threshold.
func (s *RedisSemanticStore) Lookup(ctx context.Context, ns string, vec []float32, threshold float64) (CachedResponse, bool) {
	items, err := s.rdb.LRange(ctx, s.key(ns), 0, -1).Result()
	if err != nil {
		return CachedResponse{}, false
	}

	var (
		best     float64
		bestResp CachedResponse
	)

	found := false
	for _, it := range items {
		var e semanticEntry
		if json.Unmarshal([]byte(it), &e) != nil {
			continue
		}

		sim := embed.Cosine(vec, e.Vec)
		if sim >= threshold && sim > best {
			best, bestResp, found = sim, e.Resp, true
		}
	}

	return bestResp, found
}

// Store does LPUSH of the new entry + LTRIM to the cap + refreshes the TTL (in one pipeline).
// Best-effort.
func (s *RedisSemanticStore) Store(ctx context.Context, ns string, vec []float32, resp CachedResponse, ttl time.Duration) {
	b, err := json.Marshal(semanticEntry{Vec: vec, Resp: resp})
	if err != nil {
		return
	}

	if ttl <= 0 {
		ttl = 5 * time.Minute
	}

	key := s.key(ns)
	pipe := s.rdb.Pipeline()
	pipe.LPush(ctx, key, b)
	pipe.LTrim(ctx, key, 0, int64(s.maxEntries-1))
	pipe.Expire(ctx, key, ttl)
	_, _ = pipe.Exec(ctx)
}

// Compile-time interface assertion.
