package ratelimit

import (
	"context"
	"testing"
	"time"
)

// fixedClock lets tests move across sliding-window boundaries deterministically.
type fixedClock struct{ now time.Time }

func (c *fixedClock) fn() func() time.Time { return func() time.Time { return c.now } }

func newTestStore() (*InMemoryStore, *fixedClock) {
	s := NewInMemoryStore()
	// Align to a window boundary so elapsed==0 and the previous window carries
	// full weight — makes expected values exact.
	clk := &fixedClock{now: time.Unix(1_000_000, 0)}
	s.nowFn = clk.fn()

	return s, clk
}

func TestInMemoryReserveBatch_AllOrNothing(t *testing.T) {
	s, _ := newTestStore()
	ctx := context.Background()

	a := Bucket{Key: "a", Limit: 10, Cost: 8, Window: 10 * time.Second}
	b := Bucket{Key: "b", Limit: 10, Cost: 8, Window: 10 * time.Second}

	allowed, violated, err := s.ReserveBatch(ctx, []Bucket{a, b})
	if err != nil || !allowed || violated != nil {
		t.Fatalf("first reserve: allowed=%v violated=%+v err=%v", allowed, violated, err)
	}

	// Second reserve: bucket a passes phase-1 check first? No — a is now at 8,
	// 8+8 > 10, so the batch must be rejected with NO mutation to either bucket.
	allowed, violated, err = s.ReserveBatch(ctx, []Bucket{a, b})
	if err != nil || allowed {
		t.Fatalf("second reserve should reject: allowed=%v err=%v", allowed, err)
	}

	if violated == nil || violated.Key != "a" || violated.Current != 8 {
		t.Fatalf("violation should name bucket a at current=8, got %+v", violated)
	}

	// all-or-nothing: neither bucket was charged by the rejected batch
	states, _ := s.SnapshotBatch(ctx, []Bucket{a, b})
	for _, st := range states {
		if st.Used != 8 {
			t.Errorf("bucket %s mutated by rejected batch: used=%d want 8", st.Key, st.Used)
		}
	}
}

func TestInMemoryChargeBatch_Overflow(t *testing.T) {
	s, _ := newTestStore()
	ctx := context.Background()

	b := Bucket{Key: "tpm", Limit: 100, Cost: 150, Window: time.Minute}

	out, err := s.ChargeBatch(ctx, []Bucket{b})
	if err != nil {
		t.Fatal(err)
	}

	if !out[0].Overflow || out[0].Used != 150 {
		t.Fatalf("charge past limit must flag overflow: %+v", out[0])
	}
}

func TestInMemoryReleaseBatch_ClampsAtZero(t *testing.T) {
	s, _ := newTestStore()
	ctx := context.Background()

	b := Bucket{Key: "rpm", Limit: 10, Cost: 3, Window: time.Minute}
	if ok, _, _ := s.ReserveBatch(ctx, []Bucket{b}); !ok {
		t.Fatal("reserve failed")
	}

	// release more than reserved: clamps at zero, no underflow
	if err := s.ReleaseBatch(ctx, []Bucket{{Key: "rpm", Cost: 99, Window: time.Minute}}); err != nil {
		t.Fatal(err)
	}

	states, _ := s.SnapshotBatch(ctx, []Bucket{b})
	if states[0].Used != 0 {
		t.Fatalf("release should clamp to 0, got %d", states[0].Used)
	}
}

func TestInMemorySlidingWindow_PreviousWindowDecays(t *testing.T) {
	s, clk := newTestStore()
	ctx := context.Background()

	b := Bucket{Key: "k", Limit: 100, Cost: 60, Window: 10 * time.Second}
	if ok, _, _ := s.ReserveBatch(ctx, []Bucket{b}); !ok {
		t.Fatal("reserve failed")
	}

	// Cross one boundary, land 5s (half a window) into the new one: the old 60
	// should count as floor(60 * 5/10) = 30.
	clk.now = clk.now.Add(15 * time.Second)

	states, _ := s.SnapshotBatch(ctx, []Bucket{b})
	if states[0].Used != 30 {
		t.Fatalf("half-decayed previous window: used=%d want 30", states[0].Used)
	}

	// Two full windows later everything is stale.
	clk.now = clk.now.Add(20 * time.Second)

	states, _ = s.SnapshotBatch(ctx, []Bucket{b})
	if states[0].Used != 0 {
		t.Fatalf("fully-expired window: used=%d want 0", states[0].Used)
	}
}
