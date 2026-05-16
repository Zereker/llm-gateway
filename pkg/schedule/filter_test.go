package schedule

import (
	"context"
	"errors"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// =============================================================================
// runChain
// =============================================================================

func TestRunChain_EmptyCandidates_ReturnsNil(t *testing.T) {
	got := runChain(context.Background(), []Filter{pickFirstSelector{}}, nil, &Request{})
	if got != nil {
		t.Errorf("got=%+v, want nil", got)
	}
}

func TestRunChain_FilterReturnsEmpty_EarlyExit(t *testing.T) {
	f1 := &stubFilter{name: "f1", excludeIDs: map[int64]bool{}}
	f2 := &stubFilter{name: "f2", excludeIDs: map[int64]bool{1: true, 2: true}}
	f3 := &stubFilter{name: "f3"}

	cands := []*domain.Endpoint{ep(1, 100), ep(2, 100)}
	got := runChain(context.Background(), []Filter{f1, f2, f3}, cands, &Request{})

	if got != nil {
		t.Errorf("got=%+v, want nil (f2 filters everything)", got)
	}
	if f3.calls != 0 {
		t.Errorf("f3 should not be called after f2 returns empty")
	}
}

func TestRunChain_SequentialReduction(t *testing.T) {
	f1 := &stubFilter{name: "f1", excludeIDs: map[int64]bool{1: true}}
	f2 := &stubFilter{name: "f2", excludeIDs: map[int64]bool{2: true}}

	cands := []*domain.Endpoint{ep(1, 100), ep(2, 100), ep(3, 100)}
	got := runChain(context.Background(), []Filter{f1, f2}, cands, &Request{})

	if len(got) != 1 || got[0].ID != 3 {
		t.Errorf("got=%+v, want [ep3]", got)
	}
}

// =============================================================================
// CooldownFilter
// =============================================================================

func TestCooldownFilter_EmptyCandidates_Passthrough(t *testing.T) {
	f := NewCooldownFilter(&stubCooldown{})
	got := f.Apply(context.Background(), nil, &Request{})
	if got != nil {
		t.Errorf("got=%+v, want nil", got)
	}
}

func TestCooldownFilter_NilManager_Passthrough(t *testing.T) {
	f := NewCooldownFilter(nil)
	cands := []*domain.Endpoint{ep(1, 100)}
	got := f.Apply(context.Background(), cands, &Request{})
	if len(got) != 1 {
		t.Errorf("got=%+v, want [ep1] (nil mgr passthrough)", got)
	}
}

func TestCooldownFilter_RemovesCooledEndpoints(t *testing.T) {
	cd := &stubCooldown{cooled: map[int64]bool{2: true}}
	f := NewCooldownFilter(cd)
	cands := []*domain.Endpoint{ep(1, 100), ep(2, 100), ep(3, 100)}
	got := f.Apply(context.Background(), cands, &Request{})

	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	for _, e := range got {
		if e.ID == 2 {
			t.Errorf("ep2 should be filtered (in cooldown)")
		}
	}
}

func TestCooldownFilter_FailOpen_OnRedisErr(t *testing.T) {
	cd := &stubCooldown{inCooldown: func(_ []int64) (map[int64]bool, error) {
		return nil, errors.New("redis down")
	}}
	f := NewCooldownFilter(cd)
	cands := []*domain.Endpoint{ep(1, 100), ep(2, 100)}
	got := f.Apply(context.Background(), cands, &Request{})

	if len(got) != 2 {
		t.Errorf("fail-open should preserve all candidates, got %d", len(got))
	}
}

func TestCooldownFilter_Name(t *testing.T) {
	f := NewCooldownFilter(nil)
	if f.Name() != "cooldown" {
		t.Errorf("name=%q", f.Name())
	}
}

// =============================================================================
// CooldownDurations.Get
// =============================================================================

func TestCooldownDurations_Get(t *testing.T) {
	d := CooldownDurations{
		Transient: 5,
		Capacity:  10,
		Permanent: 60,
		Invalid:   0,
		Unknown:   2,
	}
	if d.Get(ClassTransient) != 5 {
		t.Error("ClassTransient")
	}
	if d.Get(ClassCapacity) != 10 {
		t.Error("ClassCapacity")
	}
	if d.Get(ClassPermanent) != 60 {
		t.Error("ClassPermanent")
	}
	if d.Get(ClassInvalid) != 0 {
		t.Error("ClassInvalid")
	}
	if d.Get(ClassUnknown) != 2 {
		t.Error("ClassUnknown")
	}
	if d.Get(ClassSuccess) != 0 {
		t.Error("ClassSuccess should be 0 (not configured)")
	}
}
