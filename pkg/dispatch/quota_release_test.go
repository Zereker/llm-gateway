package dispatch

import (
	"context"
	"net/http/httptest"
	"testing"
)

// When the call can't even be constructed (invoke error), the endpoint was
// never contacted, so its reserve must be released.
func TestDispatcher_ReleasesReserveOnInvokeConstructionError(t *testing.T) {
	q := &recordingQuota{}
	sel := newFakeSelector(
		selResp{ep: newTestEP(1)},
		selResp{ep: nil}, // exhausted after the retry
	)
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(sel),
		WithInvokerFactory(newFakeInvokerFactory(&fakeResult{invokeErr: errFakeDep})),
		WithQuota(q),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	d.Dispatch(context.Background(), httptest.NewRecorder(), newTestInput("gpt-4"))

	if q.reserves != 1 {
		t.Fatalf("want 1 reserve, got %d", q.reserves)
	}
	if q.releases != 1 {
		t.Errorf("invoke-construction failure must release the reserve, got %d releases", q.releases)
	}
}

// A successful attempt must NOT release the reserve (the endpoint was served).
func TestDispatcher_KeepsReserveOnSuccess(t *testing.T) {
	q := &recordingQuota{}
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector(selResp{ep: newTestEP(1)})),
		WithInvokerFactory(newFakeInvokerFactory(successResult(nil, 5))),
		WithQuota(q),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	d.Dispatch(context.Background(), httptest.NewRecorder(), newTestInput("gpt-4"))

	if q.releases != 0 {
		t.Errorf("a served request must not release its reserve, got %d releases", q.releases)
	}
}
