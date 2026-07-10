package selector

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
)

// Inflight tracks how many calls picked through the scheduler are still
// pending per endpoint (Pick increments, the matching Report decrements).
//
// Scope note: the counter covers the window from Pick until the dispatcher's
// first Report for that attempt — i.e. up to the upstream's response headers,
// not the full streaming duration. That is still the window where an
// overloaded upstream queues (slow TTFB), which is the load signal P2C needs.
// Counters are per-process; each gateway replica balances its own view, which
// is the standard deployment shape for P2C.
type Inflight struct {
	mu sync.RWMutex
	m  map[int64]*atomic.Int64
}

// NewInflight constructs an empty tracker.
func NewInflight() *Inflight {
	return &Inflight{m: make(map[int64]*atomic.Int64)}
}

// Inc adds one pending call for the endpoint.
func (f *Inflight) Inc(endpointID int64) {
	f.counter(endpointID).Add(1)
}

// Dec releases one pending call; clamps at 0 so a supplementary report
// (e.g. StageStream after a success report) can't drive the counter negative.
func (f *Inflight) Dec(endpointID int64) {
	c := f.counter(endpointID)
	for {
		cur := c.Load()
		if cur <= 0 {
			return
		}
		if c.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// Get returns the current pending-call count for the endpoint.
func (f *Inflight) Get(endpointID int64) int64 {
	f.mu.RLock()
	c := f.m[endpointID]
	f.mu.RUnlock()
	if c == nil {
		return 0
	}
	return c.Load()
}

func (f *Inflight) counter(endpointID int64) *atomic.Int64 {
	f.mu.RLock()
	c := f.m[endpointID]
	f.mu.RUnlock()
	if c != nil {
		return c
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c = f.m[endpointID]; c == nil {
		c = &atomic.Int64{}
		f.m[endpointID] = c
	}
	return c
}

// P2CPicker implements power-of-two-choices selection: sample two distinct
// candidates by EffectiveWeight, then take the one with fewer pending calls.
//
// Compared to plain weighted random, P2C reacts to instantaneous load — an
// endpoint that starts queueing accumulates pending calls and immediately
// loses the pairwise comparisons, without waiting for the EMA-based Runtime
// Scoring (docs/03 §8) to catch up. The two mechanisms compose: scoring
// shifts EffectiveWeight (sampling probability), P2C breaks the tie by live
// load.
//
// Semantics preserved from WeightedRandomPicker: EffectiveWeight <= 0 is
// excluded ("soft offline"); all excluded → nil.
type P2CPicker struct {
	inflight *Inflight
	rng      *rand.Rand // nil = math/rand global (thread-safe)
}

// NewP2CPicker constructs a P2C picker reading load from the given tracker.
func NewP2CPicker(inflight *Inflight) *P2CPicker {
	return &P2CPicker{inflight: inflight}
}

// Select implements Picker.
func (p *P2CPicker) Select(_ context.Context, candidates []Candidate) *Candidate {
	live := make([]Candidate, 0, len(candidates))
	var total float64
	for _, c := range candidates {
		if c.EffectiveWeight <= 0 {
			continue
		}
		total += c.EffectiveWeight
		live = append(live, c)
	}
	switch len(live) {
	case 0:
		return nil
	case 1:
		return &live[0]
	}

	i := p.sample(live, total, -1)
	j := p.sample(live, total, i)
	return p.less(&live[i], &live[j])
}

// sample draws one index by EffectiveWeight, skipping the excluded index
// (pass -1 to allow all). live must contain >= 2 entries when skip >= 0.
func (p *P2CPicker) sample(live []Candidate, total float64, skip int) int {
	if skip >= 0 {
		total -= live[skip].EffectiveWeight
	}
	target := p.randFloat() * total
	var acc float64
	last := -1
	for i := range live {
		if i == skip {
			continue
		}
		acc += live[i].EffectiveWeight
		last = i
		if target < acc {
			return i
		}
	}
	return last // float rounding fallback
}

// less returns the candidate with fewer pending calls; ties go to the higher
// EffectiveWeight (falling back to a for stability).
func (p *P2CPicker) less(a, b *Candidate) *Candidate {
	la, lb := p.inflight.Get(a.Endpoint.ID), p.inflight.Get(b.Endpoint.ID)
	if lb < la {
		return b
	}
	if la < lb {
		return a
	}
	if b.EffectiveWeight > a.EffectiveWeight {
		return b
	}
	return a
}

func (p *P2CPicker) randFloat() float64 {
	if p.rng != nil {
		return p.rng.Float64()
	}
	return rand.Float64()
}

// Compile-time assertion.
var _ Picker = (*P2CPicker)(nil)
