package repo

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Note: TTL / LRU behavior relies on the underlying hashicorp/golang-lru v2
// expirable.LRU; this file only covers the surface of our thin Get / Set /
// Delete / Len wrapper + capacity fallback. No time-injection is done here
// (the library's internal clock isn't injectable, and testing TTL via
// time.Sleep would be slow and unnecessary).

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
	c.Delete("nope") // should not panic
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
	// capacity <= 0 should fall back to 1024, without panicking
	c := NewTTLCache[string, int](0, time.Minute)
	for i := 0; i < 100; i++ {
		c.Set(string(rune('a'+i%26)), i)
	}
	// 26 distinct keys, all should still be present
	if got := c.Len(); got != 26 {
		t.Errorf("Len=%d, want 26", got)
	}
}

// GetOrLoad tests ------------------------------------------------------------

func TestTTLCache_GetOrLoad_HitSkipsLoader(t *testing.T) {
	c := NewTTLCache[string, int](10, time.Minute)
	c.Set("a", 42)

	called := 0
	v, err := c.GetOrLoad(context.Background(), "a", func(context.Context) (int, bool, error) {
		called++
		return 0, true, nil
	})
	if err != nil || v != 42 {
		t.Fatalf("hit: v=%d err=%v, want 42 nil", v, err)
	}
	if called != 0 {
		t.Errorf("loader called %d times, want 0", called)
	}
}

func TestTTLCache_GetOrLoad_MissInvokesLoaderAndCaches(t *testing.T) {
	c := NewTTLCache[string, int](10, time.Minute)
	called := 0
	loader := func(context.Context) (int, bool, error) {
		called++
		return 100, true, nil
	}

	v, _ := c.GetOrLoad(context.Background(), "a", loader)
	if v != 100 {
		t.Fatalf("got %d, want 100", v)
	}
	// second call should hit the cache and not invoke the loader
	v, _ = c.GetOrLoad(context.Background(), "a", loader)
	if v != 100 || called != 1 {
		t.Errorf("v=%d called=%d, want 100 1", v, called)
	}
}

func TestTTLCache_GetOrLoad_NoCacheFlag(t *testing.T) {
	// when the loader returns cache=false, the result should not be written back
	c := NewTTLCache[string, int](10, time.Minute)
	called := 0
	loader := func(context.Context) (int, bool, error) {
		called++
		return 7, false, nil
	}

	v, _ := c.GetOrLoad(context.Background(), "a", loader)
	if v != 7 {
		t.Fatalf("got %d, want 7", v)
	}
	if _, ok := c.Get("a"); ok {
		t.Error("cache=false should not populate the cache")
	}
	// second call should still invoke the loader (nothing was cached)
	_, _ = c.GetOrLoad(context.Background(), "a", loader)
	if called != 2 {
		t.Errorf("loader called %d times, want 2", called)
	}
}

func TestTTLCache_GetOrLoad_ErrorNotCached(t *testing.T) {
	c := NewTTLCache[string, int](10, time.Minute)
	loaderErr := errors.New("boom")
	v, err := c.GetOrLoad(context.Background(), "a", func(context.Context) (int, bool, error) {
		return 0, true, loaderErr
	})
	if !errors.Is(err, loaderErr) {
		t.Fatalf("err=%v, want %v", err, loaderErr)
	}
	if v != 0 {
		t.Errorf("got %d, want zero", v)
	}
	if _, ok := c.Get("a"); ok {
		t.Error("err should not be cached")
	}
}

type recordingMetrics struct {
	mu      sync.Mutex
	records []string // "table:result"
}

func (m *recordingMetrics) Record(table, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, table+":"+result)
}

func (m *recordingMetrics) snapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.records))
	copy(out, m.records)
	return out
}

func TestTTLCache_Metrics_Hit_Miss_Error(t *testing.T) {
	m := &recordingMetrics{}
	c := NewTTLCache[string, int](10, time.Minute).WithMetrics("widgets", m)

	loader := func(context.Context) (int, bool, error) {
		return 42, true, nil
	}

	// first call: miss
	_, _ = c.GetOrLoad(context.Background(), "a", loader)
	// second call: hit
	_, _ = c.GetOrLoad(context.Background(), "a", loader)
	// third call: error
	_, _ = c.GetOrLoad(context.Background(), "b", func(context.Context) (int, bool, error) {
		return 0, false, errors.New("boom")
	})

	got := m.snapshot()
	want := []string{"widgets:miss", "widgets:hit", "widgets:error"}
	if len(got) != len(want) {
		t.Fatalf("records=%v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("records[%d]=%q, want %q", i, got[i], w)
		}
	}
}

func TestTTLCache_GetOrLoad_SingleflightCollapsesConcurrent(t *testing.T) {
	c := NewTTLCache[string, int](10, time.Minute)
	var calls int32
	loader := func(context.Context) (int, bool, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(50 * time.Millisecond) // simulate a SQL call
		return 99, true, nil
	}

	const N = 20
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.GetOrLoad(context.Background(), "hot", loader)
			if err != nil || v != 99 {
				t.Errorf("v=%d err=%v", v, err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("loader called %d times, want 1 (singleflight not working)", got)
	}
}

// **Regression**: a canceled leader ctx must not poison the other singleflight
// waiters. The loader must run on a WithoutCancel'd ctx — when the leader
// disconnects, the SQL call still completes normally and populates the cache.
func TestTTLCache_GetOrLoad_CanceledLeaderDoesNotPoisonWaiters(t *testing.T) {
	c := NewTTLCache[string, int](10, time.Minute)

	canceled, cancel := context.WithCancel(context.Background())
	cancel() // the leader's request ctx is now canceled (simulating a client disconnect)

	v, err := c.GetOrLoad(canceled, "hot", func(loadCtx context.Context) (int, bool, error) {
		// the ctx the loader receives should not be canceled (decoupled via WithoutCancel)
		if loadCtx.Err() != nil {
			return 0, false, loadCtx.Err()
		}
		return 42, true, nil
	})
	if err != nil {
		t.Fatalf("canceled leader ctx leaked into loader: %v", err)
	}
	if v != 42 {
		t.Fatalf("v=%d, want 42", v)
	}
	// and the result should already be cached — subsequent requests hit directly
	if got, ok := c.Get("hot"); !ok || got != 42 {
		t.Errorf("cache was not populated: ok=%v got=%d", ok, got)
	}
}

// The loader ctx carries a loaderTimeout deadline (not unbounded), so a hung
// DB won't block waiters forever.
func TestTTLCache_GetOrLoad_LoaderCtxHasDeadline(t *testing.T) {
	c := NewTTLCache[string, int](10, time.Minute)
	_, _ = c.GetOrLoad(context.Background(), "k", func(loadCtx context.Context) (int, bool, error) {
		dl, ok := loadCtx.Deadline()
		if !ok {
			t.Error("loader ctx should carry a deadline")
		}
		if remain := time.Until(dl); remain > loaderTimeout {
			t.Errorf("deadline exceeds loaderTimeout: %v", remain)
		}
		return 1, true, nil
	})
}
