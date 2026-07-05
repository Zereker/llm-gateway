package adapters

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/selector"
)

// PickerAdapter implements dispatch.Selector — a thin shell wrapping
// selector.Scheduler as a dispatch port.
//
// **Responsibility is strictly limited to pick + report**:
//   - Pick: converts eligible candidates + PickQuery into a selector.Request
//     and calls Scheduler.Pick
//   - Report: translates dispatch.Verdict into selector.Result and feeds it
//     into the Scheduler's cooldown feedback
//
// **Not handled here**: candidate fetching (owned separately by
// CandidateSource) and eligibility filtering (done by dispatch's internal
// filterEligible helper). This reflects v0.6's split of "select an endpoint"
// into 3 steps.
type PickerAdapter struct {
	sched selector.Scheduler
}

// NewSelector constructs a PickerAdapter.
func NewSelector(sched selector.Scheduler) *PickerAdapter {
	return &PickerAdapter{sched: sched}
}

// Pick implements dispatch.Selector.Pick.
func (s *PickerAdapter) Pick(ctx context.Context, eligible []*domain.Endpoint, q dispatch.PickQuery) (*domain.Endpoint, error) {
	if len(eligible) == 0 {
		return nil, nil
	}
	cands := make([]selector.Candidate, len(eligible))
	for i, ep := range eligible {
		cands[i] = selector.Candidate{Endpoint: ep, EffectiveWeight: float64(ep.Weight)}
	}
	return s.sched.Pick(ctx, &selector.Request{
		Model:      q.Model,
		Group:      q.Group,
		SessionKey: q.SessionKey,
		Candidates: cands,
		ExcludeIDs: q.Exclude,
	})
}

// Report implements dispatch.Selector.Report — translates dispatch.Verdict into selector.Result.
func (s *PickerAdapter) Report(ctx context.Context, ep *domain.Endpoint, v dispatch.Verdict) {
	s.sched.Report(ctx, ep, selector.Result{
		Class:    dispatchClassToSelector(v.Class),
		HTTPCode: v.HTTPCode,
		Reason:   v.Reason,
		Latency:  v.Latency,
	})
}

// dispatchClassToSelector maps dispatch.Class → selector.ErrorClass (1:1).
func dispatchClassToSelector(c dispatch.Class) selector.ErrorClass {
	switch c {
	case dispatch.ClassSuccess:
		return selector.ClassSuccess
	case dispatch.ClassTransient:
		return selector.ClassTransient
	case dispatch.ClassCapacity:
		return selector.ClassCapacity
	case dispatch.ClassPermanent:
		return selector.ClassPermanent
	case dispatch.ClassInvalid:
		return selector.ClassInvalid
	default:
		return selector.ClassUnknown
	}
}

// Compile-time assertion.
var _ dispatch.Selector = (*PickerAdapter)(nil)
