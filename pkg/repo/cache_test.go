package repo

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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

// GetOrLoad 测试 ------------------------------------------------------------

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
	// 第二次应该走 cache 不调 loader
	v, _ = c.GetOrLoad(context.Background(), "a", loader)
	if v != 100 || called != 1 {
		t.Errorf("v=%d called=%d, want 100 1", v, called)
	}
}

func TestTTLCache_GetOrLoad_NoCacheFlag(t *testing.T) {
	// loader 返回 cache=false 时不应回写
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
		t.Error("cache=false 不应回填")
	}
	// 第二次还应该调 loader（cache 里没东西）
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
		t.Error("err 不应缓存")
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

	// 第一次 miss
	_, _ = c.GetOrLoad(context.Background(), "a", loader)
	// 第二次 hit
	_, _ = c.GetOrLoad(context.Background(), "a", loader)
	// 第三次 error
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
		time.Sleep(50 * time.Millisecond) // 模拟 SQL
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
		t.Errorf("loader called %d times, want 1 (singleflight 失效)", got)
	}
}

// **回归**：leader 的 ctx 被取消不能毒化 singleflight 的其他 waiter。
// loader 必须跑在 WithoutCancel 的 ctx 上——leader 断连时 SQL 照常完成并回填。
func TestTTLCache_GetOrLoad_CanceledLeaderDoesNotPoisonWaiters(t *testing.T) {
	c := NewTTLCache[string, int](10, time.Minute)

	canceled, cancel := context.WithCancel(context.Background())
	cancel() // leader 的请求 ctx 已取消（模拟客户端断连）

	v, err := c.GetOrLoad(canceled, "hot", func(loadCtx context.Context) (int, bool, error) {
		// loader 拿到的 ctx 应该没被取消（已 WithoutCancel 解耦）
		if loadCtx.Err() != nil {
			return 0, false, loadCtx.Err()
		}
		return 42, true, nil
	})
	if err != nil {
		t.Fatalf("canceled leader ctx 泄漏进 loader：%v", err)
	}
	if v != 42 {
		t.Fatalf("v=%d, want 42", v)
	}
	// 且结果已回填——后续请求直接命中
	if got, ok := c.Get("hot"); !ok || got != 42 {
		t.Errorf("cache 未回填：ok=%v got=%d", ok, got)
	}
}

// loader ctx 带 loaderTimeout deadline（不是无限期），DB 挂死时 waiter 不会永久阻塞。
func TestTTLCache_GetOrLoad_LoaderCtxHasDeadline(t *testing.T) {
	c := NewTTLCache[string, int](10, time.Minute)
	_, _ = c.GetOrLoad(context.Background(), "k", func(loadCtx context.Context) (int, bool, error) {
		dl, ok := loadCtx.Deadline()
		if !ok {
			t.Error("loader ctx 应带 deadline")
		}
		if remain := time.Until(dl); remain > loaderTimeout {
			t.Errorf("deadline 超过 loaderTimeout：%v", remain)
		}
		return 1, true, nil
	})
}
