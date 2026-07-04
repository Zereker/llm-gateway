package cachebus

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// testRedis 连 REDIS_ADDR（没设就 skip，跟 MYSQL_DSN 模式一致）。
func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set; skipping cachebus pub/sub test")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis ping failed (%v); skipping", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestPublisherSubscriberRoundtrip(t *testing.T) {
	rdb := testRedis(t)
	ctx := context.Background()
	// 每个测试用独立频道，避免并发串扰。
	channel := "test:cachebus:" + t.Name()

	got := make(chan Invalidation, 1)
	sub := NewSubscriber(rdb, channel, func(inv Invalidation) { got <- inv })
	stop, err := sub.Start(ctx)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer stop()

	pub := NewPublisher(rdb, channel)
	want := Invalidation{Kind: KindAPIKey, Key: "deadbeefhash"}
	if err := pub.Invalidate(ctx, want); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case inv := <-got:
		if inv != want {
			t.Errorf("received %+v, want %+v", inv, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for invalidation")
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "nokey:", ":noverb", "novseparator"} {
		if _, ok := decode(bad); ok {
			t.Errorf("decode(%q) should have failed", bad)
		}
	}
	inv, ok := decode("apikey:abc123")
	if !ok || inv.Kind != KindAPIKey || inv.Key != "abc123" {
		t.Errorf("decode(apikey:abc123) = %+v ok=%v", inv, ok)
	}
	// key 里带冒号（hash 不会，但防御性）——只按第一个冒号切
	inv, ok = decode("apikey:a:b")
	if !ok || inv.Key != "a:b" {
		t.Errorf("decode multi-colon = %+v ok=%v", inv, ok)
	}
}

// nilPublisherNoop：nil Publisher / nil rdb 不 panic（graceful degradation）。
func TestNilPublisherNoop(t *testing.T) {
	var p *Publisher
	if err := p.Invalidate(context.Background(), Invalidation{Kind: KindAPIKey, Key: "x"}); err != nil {
		t.Errorf("nil publisher Invalidate = %v, want nil", err)
	}
	p2 := NewPublisher(nil, "")
	if err := p2.Invalidate(context.Background(), Invalidation{Kind: KindAPIKey, Key: "x"}); err != nil {
		t.Errorf("nil-rdb publisher Invalidate = %v, want nil", err)
	}
}
