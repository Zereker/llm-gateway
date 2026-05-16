package schedule

import (
	"context"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

func TestWeightedRandom_Empty_ReturnsNil(t *testing.T) {
	s := NewWeightedRandomSelector()
	got := s.Apply(context.Background(), nil, nil)
	if got != nil {
		t.Errorf("got=%+v, want nil", got)
	}
}

func TestWeightedRandom_AllZeroWeight_ReturnsNil(t *testing.T) {
	s := NewWeightedRandomSelector()
	cands := []*domain.Endpoint{ep(1, 0), ep(2, 0)}
	got := s.Apply(context.Background(), cands, nil)
	if got != nil {
		t.Errorf("got=%+v, want nil (all zero-weight)", got)
	}
}

func TestWeightedRandom_SingleEp_AlwaysPicks(t *testing.T) {
	s := NewWeightedRandomSelector()
	cands := []*domain.Endpoint{ep(1, 100)}
	for i := 0; i < 10; i++ {
		got := s.Apply(context.Background(), cands, nil)
		if len(got) != 1 || got[0].ID != 1 {
			t.Errorf("iter=%d: got=%+v, want [ep1]", i, got)
		}
	}
}

func TestWeightedRandom_DistributionRoughlyMatchesWeights(t *testing.T) {
	s := NewWeightedRandomSelector()
	// weight 90:10 → ep1 应远多于 ep2
	cands := []*domain.Endpoint{ep(1, 90), ep(2, 10)}
	const N = 1000
	count := map[int64]int{}
	for i := 0; i < N; i++ {
		got := s.Apply(context.Background(), cands, nil)
		if len(got) != 1 {
			t.Fatalf("got %d, want 1", len(got))
		}
		count[got[0].ID]++
	}
	// 宽松断言：ep1 至少占 70%，ep2 至少出现过几次
	if count[1] < 700 {
		t.Errorf("ep1 picked %d times in %d, expected >=700 (weight=90/100)", count[1], N)
	}
	if count[2] < 30 {
		t.Errorf("ep2 picked %d times in %d, expected >=30 (weight=10/100)", count[2], N)
	}
}

func TestWeightedRandom_ZeroWeightExcluded(t *testing.T) {
	s := NewWeightedRandomSelector()
	cands := []*domain.Endpoint{ep(1, 0), ep(2, 100)}
	for i := 0; i < 20; i++ {
		got := s.Apply(context.Background(), cands, nil)
		if len(got) != 1 || got[0].ID != 2 {
			t.Errorf("iter=%d got=%+v, want [ep2] (ep1 weight=0 excluded)", i, got)
		}
	}
}

func TestWeightedRandom_Name(t *testing.T) {
	s := NewWeightedRandomSelector()
	if s.Name() != "weighted_random" {
		t.Errorf("name=%q", s.Name())
	}
}
