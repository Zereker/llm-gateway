package respcache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zereker/llm-gateway/pkg/middleware"
)

func TestRedisSemanticStore_Roundtrip(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set; skipping semantic store test")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis ping: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	prefix := "test:sem:" + t.Name()
	rdb.Del(ctx, prefix+":sem:openai|m")

	s := NewRedisSemanticStore(rdb, prefix, 100)
	ns := "openai|m"

	// 存两个正交向量的条目
	s.Store(ctx, ns, []float32{1, 0, 0}, middleware.CachedResponse{StatusCode: 200, Body: []byte("weather-resp")}, time.Minute)
	s.Store(ctx, ns, []float32{0, 1, 0}, middleware.CachedResponse{StatusCode: 200, Body: []byte("code-resp")}, time.Minute)

	// 查一个接近 [1,0,0] 的向量 → 命中 weather-resp
	if r, ok := s.Lookup(ctx, ns, []float32{0.98, 0.02, 0}, 0.9); !ok || string(r.Body) != "weather-resp" {
		t.Errorf("Lookup 近 [1,0,0] = %q ok=%v, want weather-resp", r.Body, ok)
	}
	// 查一个跟两者都不像的向量 → miss
	if _, ok := s.Lookup(ctx, ns, []float32{0, 0, 1}, 0.9); ok {
		t.Error("正交向量应 miss")
	}
	// 阈值太高 → miss
	if _, ok := s.Lookup(ctx, ns, []float32{0.9, 0.1, 0}, 0.999); ok {
		t.Error("相似度低于阈值应 miss")
	}
}
