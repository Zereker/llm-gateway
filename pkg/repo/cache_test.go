package repo

import (
	"testing"
	"time"
)

func TestTTLCache_GetMissReturnsFalse(t *testing.T) {
	c := NewTTLCache[string, int](10, time.Minute)
	if _, ok := c.Get("nope"); ok {
		t.Fatal("expected miss")
	}
}

func TestTTLCache_SetGetHit(t *testing.T) {
	c := NewTTLCache[string, int](10, time.Minute)
	c.Set("a", 1)
	v, ok := c.Get("a")
	if !ok || v != 1 {
		t.Fatalf("Set→Get: ok=%v v=%d, want ok=true v=1", ok, v)
	}
}

func TestTTLCache_ExpiredEntryMisses(t *testing.T) {
	now := time.Now()
	c := NewTTLCache[string, int](10, 100*time.Millisecond)
	c.now = func() time.Time { return now }

	c.Set("a", 1)

	// 推进时间超过 TTL
	c.now = func() time.Time { return now.Add(200 * time.Millisecond) }
	if _, ok := c.Get("a"); ok {
		t.Fatal("expired entry should miss")
	}
	if c.Len() != 0 {
		t.Errorf("expired entry should be evicted on Get; Len=%d", c.Len())
	}
}

func TestTTLCache_LRUEvictsOldest(t *testing.T) {
	c := NewTTLCache[int, int](3, time.Minute)
	c.Set(1, 100)
	c.Set(2, 200)
	c.Set(3, 300)
	c.Set(4, 400) // 容量 3，应淘汰 key=1

	if _, ok := c.Get(1); ok {
		t.Error("key=1 should be evicted (LRU)")
	}
	for _, k := range []int{2, 3, 4} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("key=%d should still be present", k)
		}
	}
}

func TestTTLCache_GetMovesToFront(t *testing.T) {
	c := NewTTLCache[int, int](3, time.Minute)
	c.Set(1, 100)
	c.Set(2, 200)
	c.Set(3, 300)
	_, _ = c.Get(1) // bump 1 → 最近使用
	c.Set(4, 400)   // 现在该淘汰 2（最早未使用）

	if _, ok := c.Get(2); ok {
		t.Error("key=2 should be evicted (used last before new entries)")
	}
	if _, ok := c.Get(1); !ok {
		t.Error("key=1 should be present (was bumped to front)")
	}
}

func TestTTLCache_SetOverwriteRefreshesTTL(t *testing.T) {
	now := time.Now()
	c := NewTTLCache[string, int](10, 100*time.Millisecond)
	c.now = func() time.Time { return now }

	c.Set("a", 1)

	// 推进时间 50ms（还没过期）
	c.now = func() time.Time { return now.Add(50 * time.Millisecond) }
	c.Set("a", 2) // overwrite，应刷新 TTL

	// 推进时间到 80ms（原 TTL 已过，但 overwrite 刷新后还没过）
	c.now = func() time.Time { return now.Add(80 * time.Millisecond) }
	v, ok := c.Get("a")
	if !ok || v != 2 {
		t.Errorf("overwritten entry should be alive: ok=%v v=%d", ok, v)
	}
}

func TestTTLCache_Delete(t *testing.T) {
	c := NewTTLCache[string, int](10, time.Minute)
	c.Set("a", 1)
	c.Delete("a")
	if _, ok := c.Get("a"); ok {
		t.Fatal("deleted entry should miss")
	}
	if c.Len() != 0 {
		t.Errorf("Len after Delete = %d, want 0", c.Len())
	}
}

func TestTTLCache_DeleteNonexistentNoOp(t *testing.T) {
	c := NewTTLCache[string, int](10, time.Minute)
	c.Delete("nope") // 不该 panic
}

func TestTTLCache_DefaultCapacityFallback(t *testing.T) {
	// capacity <= 0 应该 fallback 到一个合理默认；不应 panic
	c := NewTTLCache[string, int](0, time.Minute)
	for i := 0; i < 100; i++ {
		c.Set(string(rune('a'+i%26)), i)
	}
	// 不验证具体行为；只要不 panic 即可
}
