package selector

import (
	"context"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

func p2cCandidates(weights map[int64]float64) []Candidate {
	out := make([]Candidate, 0, len(weights))
	// deterministic order for the trivial cases
	for _, id := range []int64{1, 2, 3, 4} {
		w, ok := weights[id]
		if !ok {
			continue
		}
		out = append(out, Candidate{Endpoint: &domain.Endpoint{ID: id}, EffectiveWeight: w})
	}
	return out
}

func TestInflight_IncDecClamp(t *testing.T) {
	f := NewInflight()
	if got := f.Get(1); got != 0 {
		t.Fatalf("initial Get = %d, want 0", got)
	}
	f.Inc(1)
	f.Inc(1)
	if got := f.Get(1); got != 2 {
		t.Fatalf("after 2 Inc: Get = %d, want 2", got)
	}
	f.Dec(1)
	f.Dec(1)
	f.Dec(1) // supplementary report — must clamp at 0, not go negative
	if got := f.Get(1); got != 0 {
		t.Fatalf("after 3 Dec: Get = %d, want 0 (clamped)", got)
	}
}

func TestP2CPicker_TrivialCases(t *testing.T) {
	p := NewP2CPicker(NewInflight())

	if got := p.Select(context.Background(), nil); got != nil {
		t.Error("empty candidates should return nil")
	}
	if got := p.Select(context.Background(), p2cCandidates(map[int64]float64{1: 0})); got != nil {
		t.Error("all zero-weight candidates should return nil (soft offline)")
	}
	got := p.Select(context.Background(), p2cCandidates(map[int64]float64{1: 0, 2: 5}))
	if got == nil || got.Endpoint.ID != 2 {
		t.Errorf("single live candidate should be returned, got %+v", got)
	}
}

func TestP2CPicker_PrefersLessLoaded(t *testing.T) {
	inflight := NewInflight()
	p := NewP2CPicker(inflight)

	// endpoint 1 is heavily loaded; endpoint 2 idle. With only two live
	// candidates, P2C always compares exactly this pair, so the pick must be
	// deterministic regardless of the weighted sampling.
	for range 10 {
		inflight.Inc(1)
	}
	cands := p2cCandidates(map[int64]float64{1: 100, 2: 1})
	for range 50 {
		got := p.Select(context.Background(), cands)
		if got == nil || got.Endpoint.ID != 2 {
			t.Fatalf("expected idle endpoint 2 to win every pairwise comparison, got %+v", got)
		}
	}
}

func TestP2CPicker_TieBreaksByWeight(t *testing.T) {
	p := NewP2CPicker(NewInflight())
	// equal load (both 0) → higher EffectiveWeight wins the comparison
	cands := p2cCandidates(map[int64]float64{1: 1, 2: 9})
	for range 50 {
		got := p.Select(context.Background(), cands)
		if got == nil || got.Endpoint.ID != 2 {
			t.Fatalf("tie on load should go to higher weight, got %+v", got)
		}
	}
}

func TestScheduler_InflightIncOnPickDecOnRelease(t *testing.T) {
	inflight := NewInflight()
	sched := New(Config{
		Picker:   NewP2CPicker(inflight),
		Inflight: inflight,
	})

	ep, err := sched.Pick(context.Background(), &Request{
		Model:      "m",
		Candidates: p2cCandidates(map[int64]float64{1: 1}),
	})
	if err != nil || ep == nil {
		t.Fatalf("Pick failed: ep=%v err=%v", ep, err)
	}
	if got := inflight.Get(ep.ID); got != 1 {
		t.Fatalf("after Pick: inflight = %d, want 1", got)
	}

	// A single attempt may Report twice (success + supplementary StageStream);
	// neither Report touches the counter — only Release does.
	sched.Report(context.Background(), ep, Result{Class: ClassSuccess})
	sched.Report(context.Background(), ep, Result{Class: ClassTransient})
	if got := inflight.Get(ep.ID); got != 1 {
		t.Fatalf("Report must not touch the counter: inflight = %d, want 1", got)
	}

	sched.Release(context.Background(), ep)
	if got := inflight.Get(ep.ID); got != 0 {
		t.Fatalf("after Release: inflight = %d, want 0", got)
	}
}
