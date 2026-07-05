package selector

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zereker/llm-gateway/pkg/domain"
)

func testAffinityRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set; skipping affinity test")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis ping failed (%v)", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestRedisAffinityStore_SetGet(t *testing.T) {
	rdb := testAffinityRedis(t)
	ctx := context.Background()
	s := NewRedisAffinityStore(rdb, "test:aff:"+t.Name(), time.Minute)

	if _, ok := s.Get(ctx, "sess-missing"); ok {
		t.Error("missing session should return ok=false")
	}
	s.Set(ctx, "sess1", 42)
	if id, ok := s.Get(ctx, "sess1"); !ok || id != 42 {
		t.Errorf("Get(sess1) = (%d,%v), want (42,true)", id, ok)
	}
}

func candidatesOf(ids ...int64) []Candidate {
	cs := make([]Candidate, len(ids))
	for i, id := range ids {
		cs[i] = Candidate{Endpoint: &domain.Endpoint{ID: id, Enabled: true, Weight: 100}, EffectiveWeight: 100}
	}
	return cs
}

// TestScheduler_SessionAffinity: same session sticks to the same endpoint; reselects and re-pins when the pinned endpoint is excluded.
func TestScheduler_SessionAffinity(t *testing.T) {
	rdb := testAffinityRedis(t)
	ctx := context.Background()
	rdb.Del(ctx, "test:aff2:affinity:free|sessX")

	sched := New(Config{
		Picker:   NewWeightedRandomPicker(),
		Affinity: NewRedisAffinityStore(rdb, "test:aff2", time.Minute),
	})

	req := func(exclude ...int64) *Request {
		ex := map[int64]struct{}{}
		for _, id := range exclude {
			ex[id] = struct{}{}
		}
		return &Request{Group: "free", SessionKey: "sessX", Candidates: candidatesOf(1, 2, 3), ExcludeIDs: ex}
	}

	// first time: pick one at random + pin
	first, err := sched.Pick(ctx, req())
	if err != nil || first == nil {
		t.Fatalf("pick1: %v ep=%v", err, first)
	}
	// pick 5 more times on the same session → must all stick to the same one
	for i := 0; i < 5; i++ {
		got, _ := sched.Pick(ctx, req())
		if got == nil || got.ID != first.ID {
			t.Fatalf("pick#%d = %v, want sticky to %d", i, got, first.ID)
		}
	}

	// exclude the pinned one → should reselect to a different ep (and re-pin to it)
	second, _ := sched.Pick(ctx, req(first.ID))
	if second == nil || second.ID == first.ID {
		t.Fatalf("after excluding pinned = %v, want a different ep", second)
	}
	// subsequent picks on the same session (without exclusion) should stick to the new pin (second)
	got, _ := sched.Pick(ctx, req())
	if got == nil || got.ID != second.ID {
		t.Errorf("after re-pinning = %v, want %d", got, second.ID)
	}
}

// When there's no SessionKey, affinity isn't used (normal weighted) — the affinity store isn't touched.
func TestScheduler_NoSessionKeyNoAffinity(t *testing.T) {
	rdb := testAffinityRedis(t)
	sched := New(Config{
		Picker:   NewWeightedRandomPicker(),
		Affinity: NewRedisAffinityStore(rdb, "test:aff3", time.Minute),
	})
	ep, err := sched.Pick(context.Background(), &Request{Group: "free", Candidates: candidatesOf(1)})
	if err != nil || ep == nil {
		t.Fatalf("pick without session: %v ep=%v", err, ep)
	}
}
