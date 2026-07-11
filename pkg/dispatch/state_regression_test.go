package dispatch

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// TestState_RecordSkipsExcludeOnClassUnknown is a unit-level regression:
// ClassUnknown (dependency failure / classification blind spot) must not add
// the endpoint to excluded — otherwise a single Redis blip would permanently
// drop a healthy endpoint from the subsequent candidate pool. Other classes
// still exclude normally.
func TestState_RecordSkipsExcludeOnClassUnknown(t *testing.T) {
	s := newState(newTestInput("gpt-4"), 5)
	ep := newTestEP(7)

	s.Record(ep, Verdict{Stage: StageReserve, Class: ClassUnknown, Reason: "redis blip"})
	if s.IsExcluded(ep.ID) {
		t.Fatal("ClassUnknown should not exclude the endpoint (a dependency failure is not the endpoint's fault)")
	}
	if s.Attempts() != 1 {
		t.Fatalf("attempts = %d, want 1 (unknown still counts as one attempt)", s.Attempts())
	}

	// counterpart: a genuine failure (transient) must exclude.
	s.Record(ep, Verdict{Stage: StageInvoke, Class: ClassTransient})
	if !s.IsExcluded(ep.ID) {
		t.Fatal("ClassTransient must exclude the endpoint")
	}
}

// excludeAwareSelector honors PickQuery.Exclude: an excluded endpoint is never
// returned again (fakeSelector ignores exclude, so it can't verify the real
// effect of the excluded set on candidates).
type excludeAwareSelector struct {
	ep      *domain.Endpoint
	reports []Verdict
}

func (s *excludeAwareSelector) Pick(_ context.Context, _ []*domain.Endpoint, q PickQuery) (*domain.Endpoint, error) {
	if _, excluded := q.Exclude[s.ep.ID]; excluded {
		return nil, nil
	}
	return s.ep, nil
}

func (s *excludeAwareSelector) Report(_ context.Context, _ *domain.Endpoint, v Verdict) {
	s.reports = append(s.reports, v)
}

func (s *excludeAwareSelector) Release(_ context.Context, _ *domain.Endpoint) {}

// singleEPCandidates always returns the same endpoint (a scenario shared across models).
type singleEPCandidates struct{ ep *domain.Endpoint }

func (c singleEPCandidates) ListForModel(_ context.Context, _, _ string) ([]*domain.Endpoint, error) {
	return []*domain.Endpoint{c.ep}, nil
}

// unknownThenOKQuota: the first Reserve returns a dependency failure (store
// err → ClassUnknown), then allows subsequent calls — simulating a single
// Redis blip.
type unknownThenOKQuota struct{ calls int }

func (q *unknownThenOKQuota) Reserve(_ context.Context, _ *domain.Endpoint) (*QuotaVerdict, error) {
	q.calls++
	if q.calls == 1 {
		return nil, errors.New("redis: connection reset")
	}
	return nil, nil
}

func (q *unknownThenOKQuota) ChargeUsage(_ context.Context, _ *domain.Endpoint, _ *domain.Usage) {}

// TestDispatcher_ClassUnknownEndpointStaysEligible is an end-to-end
// regression: the only endpoint hits a single store blip (ClassUnknown) and
// must still remain in the candidate pool to be reselected on the next
// attempt, rather than being permanently excluded and returning 503 for a
// healthy endpoint.
//
// Before the fix: Record unconditionally excluded → the second Pick hit the
// exclude set and returned nil → no candidates → OnExhausted → single model,
// no fallback → 503.
func TestDispatcher_ClassUnknownEndpointStaysEligible(t *testing.T) {
	ep := newTestEP(5)
	sel := &excludeAwareSelector{ep: ep}
	d := New(
		WithCandidates(singleEPCandidates{ep: ep}),
		WithSelector(sel),
		WithInvokerFactory(newFakeInvokerFactory(successResult(&domain.Usage{Total: 20}, 10))),
		WithQuota(&unknownThenOKQuota{}),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	out := d.Dispatch(context.Background(), httptest.NewRecorder(), newTestInput("gpt-4"))

	if out.Result != OutcomeStreamed {
		t.Fatalf("Result = %s, want Streamed (endpoint should be reselected and succeed after a blip)", out.Result)
	}
	if len(out.Decision.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (unknown reserve + successful invoke)", len(out.Decision.Attempts))
	}
	if len(sel.reports) != 2 {
		t.Fatalf("reports = %d, want 2 (unknown reserve + success invoke)", len(sel.reports))
	}
	if sel.reports[0].Class != ClassUnknown {
		t.Errorf("reports[0].Class = %s, want unknown (a store blip should not write a cooldown)", sel.reports[0].Class)
	}
}
