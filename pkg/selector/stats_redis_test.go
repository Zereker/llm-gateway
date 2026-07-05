package selector

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testStatsRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set; skipping RedisStatsStore test")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis ping failed (%v)", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestRedisStatsStore_EMAAndSnapshot(t *testing.T) {
	rdb := testStatsRedis(t)
	ctx := context.Background()
	prefix := "test:sched:" + t.Name()
	rdb.Del(ctx, prefix+":epstats:42")

	s := NewRedisStatsStore(rdb, prefix, 0.5, time.Hour)

	// no data → neutral snapshot
	if got := s.Snapshot(ctx, 42); got.SuccessRate != 1.0 || got.SampleCount != 0 {
		t.Fatalf("neutral snapshot = %+v, want SuccessRate=1 SampleCount=0", got)
	}

	// first record: latency 100ms success → take the value directly
	s.Record(ctx, 42, Result{Class: ClassSuccess, Latency: 100 * time.Millisecond})
	st := s.Snapshot(ctx, 42)
	if st.SampleCount != 1 || st.LatencyMs != 100 || st.SuccessRate != 1.0 {
		t.Fatalf("after 1 record = %+v, want cnt=1 lat=100 succ=1", st)
	}

	// second record: latency 300ms failure, decay=0.5 → lat=0.5*300+0.5*100=200, succ=0.5*0+0.5*1=0.5
	s.Record(ctx, 42, Result{Class: ClassTransient, Latency: 300 * time.Millisecond})
	st = s.Snapshot(ctx, 42)
	if st.SampleCount != 2 || st.LatencyMs != 200 || st.SuccessRate != 0.5 {
		t.Errorf("after 2 records = %+v, want cnt=2 lat=200 succ=0.5", st)
	}
}

// Key point: two independent stores share the same Redis → one Records, the other Snapshots and sees it.
// This is exactly the multi-replica consistency that the InMemory version cannot provide.
func TestRedisStatsStore_SharedAcrossReplicas(t *testing.T) {
	rdb := testStatsRedis(t)
	ctx := context.Background()
	prefix := "test:sched:" + t.Name()
	rdb.Del(ctx, prefix+":epstats:7")

	replicaA := NewRedisStatsStore(rdb, prefix, 0.2, time.Hour)
	replicaB := NewRedisStatsStore(rdb, prefix, 0.2, time.Hour)

	// A records
	replicaA.Record(ctx, 7, Result{Class: ClassSuccess, Latency: 50 * time.Millisecond})
	// B sees it
	st := replicaB.Snapshot(ctx, 7)
	if st.SampleCount != 1 || st.LatencyMs != 50 {
		t.Errorf("replicaB snapshot = %+v, want cnt=1 lat=50 (should be shared across replicas)", st)
	}
	// B records again, A sees the accumulated value
	replicaB.Record(ctx, 7, Result{Class: ClassSuccess, Latency: 50 * time.Millisecond})
	if st := replicaA.Snapshot(ctx, 7); st.SampleCount != 2 {
		t.Errorf("replicaA sees cnt=%d, want 2", st.SampleCount)
	}
}
