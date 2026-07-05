package respcache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zereker/llm-gateway/pkg/embed"
	"github.com/zereker/llm-gateway/pkg/middleware"
)

// RedisSemanticStore 语义缓存的 Redis 实现（middleware.SemanticCacheStore）。
//
// **索引形态**：每个 namespace 一个有界 LIST `<prefix>:sem:<ns>`,元素是
// {vec, resp} JSON。Lookup 拉全表在 Go 里算 cosine（暴力 KNN）——namespace 内条目上
// 限 maxEntries（LTRIM），个位数到几百量级 O(N) 可忽略。真要上规模换 RediSearch
// 向量索引,接口不变。
type RedisSemanticStore struct {
	rdb        *redis.Client
	prefix     string
	maxEntries int
}

// NewRedisSemanticStore 构造；prefix 空用默认；maxEntries<=0 用 500。
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
	Vec  []float32                 `json:"vec"`
	Resp middleware.CachedResponse `json:"resp"`
}

// Lookup 暴力扫 namespace 里全部条目，返回 cosine 最高且 ≥ threshold 的响应。
func (s *RedisSemanticStore) Lookup(ctx context.Context, ns string, vec []float32, threshold float64) (middleware.CachedResponse, bool) {
	items, err := s.rdb.LRange(ctx, s.key(ns), 0, -1).Result()
	if err != nil {
		return middleware.CachedResponse{}, false
	}
	var best float64
	var bestResp middleware.CachedResponse
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

// Store LPUSH 新条目 + LTRIM 到上限 + 刷 TTL（一个 pipeline）。best-effort。
func (s *RedisSemanticStore) Store(ctx context.Context, ns string, vec []float32, resp middleware.CachedResponse, ttl time.Duration) {
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

// 编译期断言。
var _ middleware.SemanticCacheStore = (*RedisSemanticStore)(nil)
