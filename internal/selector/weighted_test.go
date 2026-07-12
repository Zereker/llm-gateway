package selector

import (
	"context"
	"testing"
)

func TestWeightedRandom_Empty_ReturnsNil(t *testing.T) {
	s := NewWeightedRandomPicker()
	if got := s.Select(context.Background(), nil); got != nil {
		t.Errorf("got=%+v, want nil", got)
	}
}

func TestWeightedRandom_AllZeroWeight_ReturnsNil(t *testing.T) {
	s := NewWeightedRandomPicker()
	got := s.Select(context.Background(), []Candidate{
		{Endpoint: ep(1, 0), EffectiveWeight: 0},
		{Endpoint: ep(2, 0), EffectiveWeight: 0},
	})
	if got != nil {
		t.Errorf("got=%+v, want nil", got)
	}
}

func TestWeightedRandom_SingleEp_AlwaysPicks(t *testing.T) {
	s := NewWeightedRandomPicker()
	cands := candidates(ep(1, 100))
	for i := 0; i < 10; i++ {
		got := s.Select(context.Background(), cands)
		if got == nil || got.Endpoint.ID != 1 {
			t.Errorf("iter=%d: got=%+v, want ep1", i, got)
		}
	}
}

func TestWeightedRandom_DistributionRoughlyMatchesWeights(t *testing.T) {
	s := NewWeightedRandomPicker()
	cands := candidates(ep(1, 90), ep(2, 10))
	const N = 1000
	count := map[int64]int{}
	for i := 0; i < N; i++ {
		got := s.Select(context.Background(), cands)
		if got == nil {
			t.Fatal("nil pick")
		}
		count[got.Endpoint.ID]++
	}
	if count[1] < 700 {
		t.Errorf("ep1 picked %d times in %d, expected >=700", count[1], N)
	}
	if count[2] < 30 {
		t.Errorf("ep2 picked %d times in %d, expected >=30", count[2], N)
	}
}

func TestWeightedRandom_ZeroWeightExcluded(t *testing.T) {
	s := NewWeightedRandomPicker()
	cands := []Candidate{
		{Endpoint: ep(1, 0), EffectiveWeight: 0},
		{Endpoint: ep(2, 100), EffectiveWeight: 100},
	}
	for i := 0; i < 20; i++ {
		got := s.Select(context.Background(), cands)
		if got == nil || got.Endpoint.ID != 2 {
			t.Errorf("got=%+v, want ep2", got)
		}
	}
}

func TestWeightedRandom_UsesEffectiveWeight_NotStaticWeight(t *testing.T) {
	// key test: docs/03 §4 — WeightedRandom must be based on EffectiveWeight
	// ep1 static weight=100, EffectiveWeight=0 → should be excluded
	// ep2 static weight=10, EffectiveWeight=100 → should be selected
	s := NewWeightedRandomPicker()
	cands := []Candidate{
		{Endpoint: ep(1, 100), EffectiveWeight: 0},
		{Endpoint: ep(2, 10), EffectiveWeight: 100},
	}
	for i := 0; i < 20; i++ {
		got := s.Select(context.Background(), cands)
		if got == nil || got.Endpoint.ID != 2 {
			t.Errorf("got=%+v, want ep2 (EffectiveWeight takes priority over static)", got)
		}
	}
}
