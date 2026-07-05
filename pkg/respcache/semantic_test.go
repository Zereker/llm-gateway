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

	// Store two entries with orthogonal vectors
	s.Store(ctx, ns, []float32{1, 0, 0}, middleware.CachedResponse{StatusCode: 200, Body: []byte("weather-resp")}, time.Minute)
	s.Store(ctx, ns, []float32{0, 1, 0}, middleware.CachedResponse{StatusCode: 200, Body: []byte("code-resp")}, time.Minute)

	// Query a vector close to [1,0,0] → hits weather-resp
	if r, ok := s.Lookup(ctx, ns, []float32{0.98, 0.02, 0}, 0.9); !ok || string(r.Body) != "weather-resp" {
		t.Errorf("Lookup near [1,0,0] = %q ok=%v, want weather-resp", r.Body, ok)
	}
	// Query a vector that resembles neither → miss
	if _, ok := s.Lookup(ctx, ns, []float32{0, 0, 1}, 0.9); ok {
		t.Error("an orthogonal vector should miss")
	}
	// Threshold too high → miss
	if _, ok := s.Lookup(ctx, ns, []float32{0.9, 0.1, 0}, 0.999); ok {
		t.Error("similarity below threshold should miss")
	}
}
