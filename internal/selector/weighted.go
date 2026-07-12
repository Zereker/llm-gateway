package selector

import (
	"context"
	"math/rand"
)

// Picker is the final selection step after Filter / Scorer: picks 1 candidate by EffectiveWeight.
//
// Design philosophy (docs/03 §4 §8):
//   - WeightedRandom must be based on Candidate.EffectiveWeight, not Endpoint.Weight
//   - trivially returns the sole candidate when only 1 remains
//   - all EffectiveWeight=0 or empty → returns nil (dispatch.FallbackPolicy.OnExhausted handles it)
//
// Implementations MUST be safe for concurrent use.
type Picker interface {
	Select(ctx context.Context, candidates []Candidate) *Candidate
}

// WeightedRandomPicker picks 1 by an EffectiveWeight probability distribution.
//
// weight=0 → excluded (the "soft offline" semantics for admin purposes).
// all 0 → returns nil.
type WeightedRandomPicker struct {
	rng *rand.Rand // nil = use the math/rand global
}

// NewWeightedRandomPicker constructs a selector.
//
// When rng=nil, uses the math/rand global (thread-safe, Go 1.20+ auto per-goroutine seed).
func NewWeightedRandomPicker() *WeightedRandomPicker {
	return &WeightedRandomPicker{}
}

// Select picks 1 by EffectiveWeight-weighted random.
func (s *WeightedRandomPicker) Select(_ context.Context, candidates []Candidate) *Candidate {
	if len(candidates) == 0 {
		return nil
	}

	var total float64

	live := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.EffectiveWeight <= 0 {
			continue
		}

		total += c.EffectiveWeight
		live = append(live, c)
	}

	if len(live) == 0 || total == 0 {
		return nil
	}

	if len(live) == 1 {
		return &live[0]
	}

	target := s.randFloat() * total

	var acc float64
	for i := range live {
		acc += live[i].EffectiveWeight
		if target < acc {
			return &live[i]
		}
	}
	// mathematically guaranteed not to reach here; fallback
	return &live[len(live)-1]
}

func (s *WeightedRandomPicker) randFloat() float64 {
	if s.rng != nil {
		return s.rng.Float64()
	}

	return rand.Float64()
}
