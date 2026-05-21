package repo

import (
	"testing"
	"time"
)

// 注：TTL / LRU 行为依赖底层 hashicorp/golang-lru v2 expirable.LRU；本文件只覆盖
// 我们薄包装的 Get / Set / Delete / Len API 表面 + capacity fallback。
// 不再做 time-injection（库内部时钟不可注入；TTL 用 time.Sleep 测会慢，没必要）。

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

func TestTTLCache_SetOverwrites(t *testing.T) {
	c := NewTTLCache[string, int](10, time.Minute)
	c.Set("a", 1)
	c.Set("a", 2)
	v, _ := c.Get("a")
	if v != 2 {
		t.Errorf("overwrite Get=%d, want 2", v)
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

func TestTTLCache_LenTracksEntries(t *testing.T) {
	c := NewTTLCache[int, int](10, time.Minute)
	c.Set(1, 100)
	c.Set(2, 200)
	if got := c.Len(); got != 2 {
		t.Errorf("Len=%d, want 2", got)
	}
}

func TestTTLCache_DefaultCapacityFallback(t *testing.T) {
	// capacity <= 0 应该 fallback 到 1024，且不 panic
	c := NewTTLCache[string, int](0, time.Minute)
	for i := 0; i < 100; i++ {
		c.Set(string(rune('a'+i%26)), i)
	}
	// 26 个 distinct key，全部应该还在
	if got := c.Len(); got != 26 {
		t.Errorf("Len=%d, want 26", got)
	}
}
