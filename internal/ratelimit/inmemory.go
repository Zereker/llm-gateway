package ratelimit

import (
	"context"
	"sync"
	"time"
)

// InMemoryStore is the process-local Store implementation, selected by
// `rate_limit.driver: inmemory`. It mirrors RedisStore's sliding-window math
// exactly (two window slots per key, previous window weighted by the
// unelapsed fraction) so switching drivers never changes limiting behavior —
// only its scope: counters live in this process, so limits are enforced
// **per replica**, not fleet-wide. Use it for single-replica deployments,
// local development, and tests; multi-replica production must stay on redis
// (docs/04 §5).
type InMemoryStore struct {
	mu    sync.Mutex
	slots map[string]*slotPair

	// nowFn is the time source; replaceable in tests to cross window
	// boundaries deterministically.
	nowFn func() time.Time
}

// slotPair is one key's sliding-window state: the count for the window
// starting at curStart, plus the previous window's count. Rolling forward is
// O(1) on access (see roll), so no background sweeper is needed; memory is
// bounded by the number of distinct live bucket keys.
type slotPair struct {
	curStart int64 // unix seconds, aligned to the bucket's window
	cur      uint32
	prev     uint32
}

// NewInMemoryStore builds an empty process-local store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		slots: make(map[string]*slotPair),
		nowFn: time.Now,
	}
}

// roll returns the key's slot pair normalized to the window containing now,
// shifting cur into prev (or zeroing both) as window boundaries pass.
func (s *InMemoryStore) roll(key string, window time.Duration, now int64) *slotPair {
	w := int64(window / time.Second)
	if w <= 0 {
		w = 1
	}

	curStart := now / w * w

	p, ok := s.slots[key]
	if !ok {
		p = &slotPair{curStart: curStart}
		s.slots[key] = p

		return p
	}

	switch p.curStart {
	case curStart:
		// still in the same window
	case curStart - w:
		// crossed exactly one boundary: current becomes previous
		p.prev, p.cur, p.curStart = p.cur, 0, curStart
	default:
		// idle for more than a full window: both slots are stale
		p.prev, p.cur, p.curStart = 0, 0, curStart
	}

	return p
}

// effective computes the sliding-window value RedisStore's Lua uses:
// cur + floor(prev * (window-elapsed)/window).
func (p *slotPair) effective(window time.Duration, now int64) uint32 {
	w := int64(window / time.Second)
	if w <= 0 {
		w = 1
	}

	elapsed := now - p.curStart

	return p.cur + uint32(int64(p.prev)*(w-elapsed)/w)
}

// ReserveBatch implements Store: phase 1 checks every bucket, phase 2 applies
// every cost — all under one lock, so the all-or-nothing guarantee matches
// the Lua script's atomicity.
func (s *InMemoryStore) ReserveBatch(_ context.Context, buckets []Bucket) (bool, *BucketViolation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowFn().Unix()

	pairs := make([]*slotPair, len(buckets))
	for i, b := range buckets {
		p := s.roll(b.Key, b.Window, now)
		if eff := p.effective(b.Window, now); eff+b.Cost > b.Limit {
			w := int64(b.Window / time.Second)
			retry := time.Duration(w-(now-p.curStart)) * time.Second

			return false, &BucketViolation{Key: b.Key, Limit: b.Limit, Current: eff, RetryAfter: retry}, nil
		}

		pairs[i] = p
	}

	for i, b := range buckets {
		pairs[i].cur += b.Cost
	}

	return true, nil, nil
}

// ChargeBatch implements Store: writes real usage unconditionally, flagging
// Overflow when the bucket ends up past its limit.
func (s *InMemoryStore) ChargeBatch(_ context.Context, buckets []Bucket) ([]BucketChargeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowFn().Unix()

	out := make([]BucketChargeResult, len(buckets))
	for i, b := range buckets {
		p := s.roll(b.Key, b.Window, now)
		p.cur += b.Cost
		used := p.effective(b.Window, now)
		out[i] = BucketChargeResult{Key: b.Key, Used: used, Limit: b.Limit, Overflow: used > b.Limit}
	}

	return out, nil
}

// ReleaseBatch implements Store: refunds a prior reservation from the current
// window, clamped at zero (same best-effort semantics as the Lua script).
func (s *InMemoryStore) ReleaseBatch(_ context.Context, buckets []Bucket) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowFn().Unix()

	for _, b := range buckets {
		p := s.roll(b.Key, b.Window, now)
		if p.cur >= b.Cost {
			p.cur -= b.Cost
		} else {
			p.cur = 0
		}
	}

	return nil
}

// SnapshotBatch implements Store: read-only view of each bucket.
func (s *InMemoryStore) SnapshotBatch(_ context.Context, buckets []Bucket) ([]BucketState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowFn().Unix()

	out := make([]BucketState, len(buckets))
	for i, b := range buckets {
		p := s.roll(b.Key, b.Window, now)
		used := p.effective(b.Window, now)

		remaining := uint32(0)
		if b.Limit > used {
			remaining = b.Limit - used
		}

		w := int64(b.Window / time.Second)
		out[i] = BucketState{
			Key:       b.Key,
			Used:      used,
			Limit:     b.Limit,
			Remaining: remaining,
			ResetAt:   time.Unix(p.curStart+w, 0),
		}
	}

	return out, nil
}
