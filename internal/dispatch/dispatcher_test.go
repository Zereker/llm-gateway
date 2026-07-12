package dispatch

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
	"github.com/zereker/llm-gateway/internal/trace"
)

// captureTracer records every StartSpan / SetAttribute / End / Log call for assertions.
type captureTracer struct {
	mu     sync.Mutex
	spans  []*captureSpan
	events []captureEvent
}

type captureSpan struct {
	name  string
	attrs map[string]any
	ended bool
}

type captureEvent struct {
	name    string
	payload any
}

func (t *captureTracer) StartSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sp := &captureSpan{name: name, attrs: map[string]any{}}
	t.spans = append(t.spans, sp)
	return ctx, &captureSpanHandle{t: t, sp: sp}
}

func (t *captureTracer) Log(ctx context.Context, name string, payload any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, captureEvent{name: name, payload: payload})
}

type captureSpanHandle struct {
	t  *captureTracer
	sp *captureSpan
}

func (h *captureSpanHandle) SetAttribute(k string, v any) {
	h.t.mu.Lock()
	defer h.t.mu.Unlock()
	h.sp.attrs[k] = v
}

func (h *captureSpanHandle) End() {
	h.t.mu.Lock()
	defer h.t.mu.Unlock()
	h.sp.ended = true
}

// =============================================================================
// Dispatcher end-to-end behavior tests
// =============================================================================

// TestDispatcher_HappyPath: first select succeeds, verdict success → Stream.
func TestDispatcher_HappyPath(t *testing.T) {
	ep := newTestEP(1)
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector(selResp{ep: ep})),
		WithInvokerFactory(newFakeInvokerFactory(successResult(&domain.Usage{Total: 100}, 50))),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	in := newTestInput("gpt-4")
	w := httptest.NewRecorder()
	out := d.Dispatch(context.Background(), w, in)

	if out.Result != OutcomeStreamed {
		t.Fatalf("want OutcomeStreamed, got %s", out.Result)
	}
	if out.Usage == nil || out.Usage.Total != 100 {
		t.Fatalf("usage missing or wrong: %+v", out.Usage)
	}
	if out.TTFTMs != 50 {
		t.Fatalf("want TTFTMs=50, got %d", out.TTFTMs)
	}
	if out.RoutedModel == nil || out.RoutedModel.Model != "gpt-4" {
		t.Fatalf("RoutedModelService not set to gpt-4: %+v", out.RoutedModel)
	}
	if out.Decision == nil || len(out.Decision.Attempts) != 1 {
		t.Fatalf("decision missing or wrong attempts: %+v", out.Decision)
	}
	if out.Decision.Attempts[0].Outcome != domain.AttemptSuccess {
		t.Fatalf("want success outcome, got %s", out.Decision.Attempts[0].Outcome)
	}
}

// TestDispatcher_InvalidAbortsImmediately: invalid → 400, no retry.
func TestDispatcher_InvalidAbortsImmediately(t *testing.T) {
	ep := newTestEP(1)
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector(selResp{ep: ep})),
		WithInvokerFactory(newFakeInvokerFactory(invalidResult())),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	in := newTestInput("gpt-4")
	w := httptest.NewRecorder()
	out := d.Dispatch(context.Background(), w, in)

	if out.Result != OutcomeInvalid {
		t.Fatalf("want OutcomeInvalid, got %s", out.Result)
	}
	if out.HTTPCode != 400 {
		t.Fatalf("want 400, got %d", out.HTTPCode)
	}
	if out.Decision == nil || len(out.Decision.Attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %+v", out.Decision)
	}
	if out.Decision.Attempts[0].Outcome != domain.AttemptFail {
		t.Fatalf("want fail outcome, got %s", out.Decision.Attempts[0].Outcome)
	}
}

// TestDispatcher_RetryUntilSuccess: transient → continue → success。
func TestDispatcher_RetryUntilSuccess(t *testing.T) {
	ep1 := newTestEP(1)
	ep2 := newTestEP(2)
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector(
			selResp{ep: ep1},
			selResp{ep: ep2},
		)),
		WithInvokerFactory(newFakeInvokerFactory(
			transientResult(),
			successResult(&domain.Usage{Total: 50}, 30),
		)),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	in := newTestInput("gpt-4")
	out := d.Dispatch(context.Background(), httptest.NewRecorder(), in)

	if out.Result != OutcomeStreamed {
		t.Fatalf("want OutcomeStreamed, got %s (reason=%s)", out.Result, out.Reason)
	}
	if len(out.Decision.Attempts) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(out.Decision.Attempts))
	}
	if out.Decision.Attempts[0].Outcome != domain.AttemptFallback {
		t.Fatalf("attempt[0] outcome want fallback, got %s", out.Decision.Attempts[0].Outcome)
	}
	if out.Decision.Attempts[1].Outcome != domain.AttemptSuccess {
		t.Fatalf("attempt[1] outcome want success, got %s", out.Decision.Attempts[1].Outcome)
	}
}

// TestDispatcher_AttemptsExhausted: transient all the way to cap → NoEndpoint 503.
func TestDispatcher_AttemptsExhausted(t *testing.T) {
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector(
			selResp{ep: newTestEP(1)},
			selResp{ep: newTestEP(2)},
		)),
		WithInvokerFactory(newFakeInvokerFactory(transientResult(), transientResult())),
		WithCap(HeaderAttemptCap{Default: 2}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	in := newTestInput("gpt-4")
	out := d.Dispatch(context.Background(), httptest.NewRecorder(), in)

	if out.Result != OutcomeNoEndpoint {
		t.Fatalf("want OutcomeNoEndpoint, got %s (reason=%s)", out.Result, out.Reason)
	}
	if out.HTTPCode != 503 {
		t.Fatalf("want 503, got %d", out.HTTPCode)
	}
	if len(out.Decision.Attempts) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(out.Decision.Attempts))
	}
}

// TestDispatcher_FallbackToNextModel: primary candidates exhausted → switch fallback → success.
func TestDispatcher_FallbackToNextModel(t *testing.T) {
	ep := newTestEP(10)
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector(
			selResp{ep: nil}, // primary candidates exhausted
			selResp{ep: ep},  // fallback model has candidates
		)),
		WithInvokerFactory(newFakeInvokerFactory(successResult(&domain.Usage{Total: 200}, 80))),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	in := newTestInput("gpt-4", "gpt-3.5") // primary + 1 fallback
	out := d.Dispatch(context.Background(), httptest.NewRecorder(), in)

	if out.Result != OutcomeStreamed {
		t.Fatalf("want OutcomeStreamed, got %s (reason=%s)", out.Result, out.Reason)
	}
	if out.RoutedModel.Model != "gpt-3.5" {
		t.Fatalf("want routed gpt-3.5, got %s", out.RoutedModel.Model)
	}
	if len(out.Decision.Attempts) != 1 {
		t.Fatalf("want 1 attempt, got %d", len(out.Decision.Attempts))
	}
	if out.Decision.Attempts[0].AttemptRole != domain.AttemptRoleFallback {
		t.Fatalf("want fallback role, got %s", out.Decision.Attempts[0].AttemptRole)
	}
}

// TestDispatcher_AllModelsExhausted: all model candidates exhausted → NoEndpoint 503.
func TestDispatcher_AllModelsExhausted(t *testing.T) {
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector(
			selResp{ep: nil},
			selResp{ep: nil},
		)),
		WithInvokerFactory(newFakeInvokerFactory()), // never invoked
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	in := newTestInput("gpt-4", "gpt-3.5")
	out := d.Dispatch(context.Background(), httptest.NewRecorder(), in)

	if out.Result != OutcomeNoEndpoint {
		t.Fatalf("want OutcomeNoEndpoint, got %s (reason=%s)", out.Result, out.Reason)
	}
	if out.HTTPCode != 503 {
		t.Fatalf("want 503, got %d", out.HTTPCode)
	}
}

// TestDispatcher_SelectorDepFail: Selector.Select returns err → DepFail 503.
func TestDispatcher_SelectorDepFail(t *testing.T) {
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector(selResp{err: errFakeDep})),
		WithInvokerFactory(newFakeInvokerFactory()),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	in := newTestInput("gpt-4")
	out := d.Dispatch(context.Background(), httptest.NewRecorder(), in)

	if out.Result != OutcomeDepFail {
		t.Fatalf("want OutcomeDepFail, got %s (reason=%s)", out.Result, out.Reason)
	}
	if out.HTTPCode != 503 {
		t.Fatalf("want 503, got %d", out.HTTPCode)
	}
}

// TestDispatcher_InvokerDepFail: Invoker.Invoke returns err → goes through the
// transient retry flow (Record + Report + Continue); once candidates are
// exhausted → NoEndpoint 503.
// (The old behavior was to Abort DepFail directly, bypassing cooldown / retry
// — fixed after review, no longer the case.)
func TestDispatcher_InvokerDepFail(t *testing.T) {
	r := &fakeResult{invokeErr: errFakeDep}
	sel := newFakeSelector(
		selResp{ep: newTestEP(1)},
		selResp{ep: nil}, // candidates exhausted after retry
	)
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(sel),
		WithInvokerFactory(newFakeInvokerFactory(r)),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	in := newTestInput("gpt-4")
	out := d.Dispatch(context.Background(), httptest.NewRecorder(), in)

	if out.Result != OutcomeNoEndpoint {
		t.Fatalf("want OutcomeNoEndpoint (exhausted after retry), got %s (reason=%s)", out.Result, out.Reason)
	}
	if len(sel.reports) != 1 || sel.reports[0].Class != ClassTransient {
		t.Errorf("invoke err should produce one transient Report, got %+v", sel.reports)
	}
}

// TestDispatcher_TerminalNonRetryable: a permanent error is treated as retryable
// by DefaultRetry (switch endpoint and continue).
//
// Note: under DefaultRetry's semantics, permanent is retryable (switching ep
// might succeed); only invalid aborts directly. So this case actually retries
// until attempts are exhausted.
func TestDispatcher_TerminalNonRetryable(t *testing.T) {
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector(
			selResp{ep: newTestEP(1)},
			selResp{ep: newTestEP(2)},
		)),
		WithInvokerFactory(newFakeInvokerFactory(permanentResult(), permanentResult())),
		WithCap(HeaderAttemptCap{Default: 2}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	in := newTestInput("gpt-4")
	out := d.Dispatch(context.Background(), httptest.NewRecorder(), in)

	if out.Result != OutcomeNoEndpoint {
		t.Fatalf("want OutcomeNoEndpoint (attempts exhausted), got %s", out.Result)
	}
}

// TestDispatcher_PanicsOnMissingDeps: New() should panic when a dependency is missing.
func TestDispatcher_PanicsOnMissingDeps(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
	}{
		{"missing candidates", []Option{WithSelector(newFakeSelector()), WithInvokerFactory(newFakeInvokerFactory()), WithCap(HeaderAttemptCap{Default: 3}), WithRetry(DefaultRetry{}), WithFallback(ModelChainFallback{})}},
		{"missing selector", []Option{WithCandidates(fakeCandidates{}), WithInvokerFactory(newFakeInvokerFactory()), WithCap(HeaderAttemptCap{Default: 3}), WithRetry(DefaultRetry{}), WithFallback(ModelChainFallback{})}},
		{"missing invoker", []Option{WithCandidates(fakeCandidates{}), WithSelector(newFakeSelector()), WithCap(HeaderAttemptCap{Default: 3}), WithRetry(DefaultRetry{}), WithFallback(ModelChainFallback{})}},
		{"missing cap", []Option{WithCandidates(fakeCandidates{}), WithSelector(newFakeSelector()), WithInvokerFactory(newFakeInvokerFactory()), WithRetry(DefaultRetry{}), WithFallback(ModelChainFallback{})}},
		{"missing retry", []Option{WithCandidates(fakeCandidates{}), WithSelector(newFakeSelector()), WithInvokerFactory(newFakeInvokerFactory()), WithCap(HeaderAttemptCap{Default: 3}), WithFallback(ModelChainFallback{})}},
		{"missing fallback", []Option{WithCandidates(fakeCandidates{}), WithSelector(newFakeSelector()), WithInvokerFactory(newFakeInvokerFactory()), WithCap(HeaderAttemptCap{Default: 3}), WithRetry(DefaultRetry{})}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic")
				}
			}()
			New(tc.opts...)
		})
	}
}

// TestDispatcher_TracerSpansHappyPath: with WithTracer injected, the happy path
// produces two spans, dispatch.request + dispatch.attempt, whose attrs include
// model / endpoint / verdict.
func TestDispatcher_TracerSpansHappyPath(t *testing.T) {
	ep := newTestEP(1)
	tr := &captureTracer{}
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector(selResp{ep: ep})),
		WithInvokerFactory(newFakeInvokerFactory(successResult(&domain.Usage{Total: 100}, 50))),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
		WithTracer(tr),
	)

	d.Dispatch(context.Background(), httptest.NewRecorder(), newTestInput("gpt-4"))

	if len(tr.spans) != 2 {
		t.Fatalf("want 2 spans (request+attempt), got %d", len(tr.spans))
	}
	if tr.spans[0].name != "dispatch.request" {
		t.Errorf("spans[0].name=%q", tr.spans[0].name)
	}
	if tr.spans[1].name != "dispatch.attempt" {
		t.Errorf("spans[1].name=%q", tr.spans[1].name)
	}
	for i, sp := range tr.spans {
		if !sp.ended {
			t.Errorf("span[%d] %q not ended", i, sp.name)
		}
	}
	if got := tr.spans[1].attrs["endpoint.id"]; got != "1" {
		t.Errorf("attempt span endpoint.id=%v, want \"1\"", got)
	}
	if got := tr.spans[1].attrs["verdict.stage"]; got != "invoke" {
		t.Errorf("attempt span verdict.stage=%v, want \"invoke\"", got)
	}
	if got := tr.spans[0].attrs["dispatch.outcome"]; got != "streamed" {
		t.Errorf("request span outcome=%v, want \"streamed\"", got)
	}
}

// TestDispatcher_DecisionAlwaysFilled_NoAttempts verifies the Outcome.Decision
// contract: **even when not a single attempt ran** (no eligible / no candidate /
// cap=0), Decision must still be filled in, so audit / log / metric code
// doesn't need to special-case nil.
//
// Prior bug: state.finalize() returned immediately when len(decisions)==0,
// leaving Decision nil.
func TestDispatcher_DecisionAlwaysFilled_NoAttempts(t *testing.T) {
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector(selResp{ep: nil})), // picker returns nil directly
		WithInvokerFactory(newFakeInvokerFactory()),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	in := newTestInput("gpt-4") // single model; no fallback either; straight to NoEndpoint
	out := d.Dispatch(context.Background(), httptest.NewRecorder(), in)

	if out.Result != OutcomeNoEndpoint {
		t.Fatalf("want OutcomeNoEndpoint, got %s", out.Result)
	}
	if out.Decision == nil {
		t.Fatal("Decision should always be filled, got nil")
	}
	if out.Decision.Model != "gpt-4" {
		t.Errorf("Decision.Model = %q, want gpt-4", out.Decision.Model)
	}
	if out.Decision.RoutedModel != "gpt-4" {
		// when routing didn't succeed, the audit routed field falls back to primary, for easier downstream joins
		t.Errorf("Decision.RoutedModel = %q, want primary fallback gpt-4", out.Decision.RoutedModel)
	}
	if len(out.Decision.Attempts) != 0 {
		t.Errorf("Decision.Attempts = %d, want 0 (no attempts ran)", len(out.Decision.Attempts))
	}
	if out.Decision.DurationMs < 0 {
		t.Errorf("Decision.DurationMs = %d, want >= 0", out.Decision.DurationMs)
	}
}

// =============================================================================
// Review-fix regression tests: dispatch error semantics
// =============================================================================

// Stream interrupted after 200: Result is still Streamed (status code already
// written), but (1) the selector must receive a StageStream/transient report
// (otherwise a bad endpoint would be counted as 100% success), and (2) the
// audited attempt must not be marked success.
func TestDispatcher_StreamErrorReportedAndNotMarkedSuccess(t *testing.T) {
	ep := newTestEP(1)
	sel := newFakeSelector(selResp{ep: ep})
	res := successResult(&domain.Usage{Total: 10}, 5)
	res.streamRep.Err = errors.New("connection reset mid-SSE")

	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(sel),
		WithInvokerFactory(newFakeInvokerFactory(res)),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)
	out := d.Dispatch(context.Background(), httptest.NewRecorder(), newTestInput("gpt-4"))

	if out.Result != OutcomeStreamed {
		t.Fatalf("Result = %s, want Streamed (status code already written, cannot roll back)", out.Result)
	}
	// Two Reports: invoke success + stream transient
	if len(sel.reports) != 2 {
		t.Fatalf("selector received %d Report(s), want 2 (invoke + stream)", len(sel.reports))
	}
	if sel.reports[1].Stage != StageStream || sel.reports[1].Class != ClassTransient {
		t.Errorf("stream failure report = {%s, %s}, want {stream, transient}",
			sel.reports[1].Stage, sel.reports[1].Class)
	}
	// audited attempt not marked success
	if got := out.Decision.Attempts[0].Outcome; got != domain.AttemptFail {
		t.Errorf("attempt outcome = %s, want fail (stream was not fully delivered)", got)
	}
}

// Client actively disconnects mid-stream (request ctx canceled): the status
// code was already written, but the endpoint is healthy — we must not add a
// stream-transient report to penalize the endpoint (triggering cooldown) just
// because the client disconnected, otherwise "clients cancelling frequently"
// would wrongly hurt a healthy endpoint. Only the pre-stream success Report
// is kept.
func TestDispatcher_ClientAbortDoesNotPenalizeEndpoint(t *testing.T) {
	ep := newTestEP(1)
	sel := newFakeSelector(selResp{ep: ep})
	res := successResult(&domain.Usage{Total: 10}, 5)
	res.streamRep.Err = context.Canceled // stream interrupted, but caused by client disconnect

	// client disconnects mid-stream: headers already written, ctx cancels
	// while StreamTo is running (an entry-canceled ctx now aborts pre-select
	// with 499 instead — see TestDispatcher_ClientGoneBeforeSelectAbortsImmediately)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res.onStream = cancel

	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(sel),
		WithInvokerFactory(newFakeInvokerFactory(res)),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)
	out := d.Dispatch(ctx, httptest.NewRecorder(), newTestInput("gpt-4"))

	if out.Result != OutcomeStreamed {
		t.Fatalf("Result = %s, want Streamed (status code already written)", out.Result)
	}
	// only the pre-stream success report is present; no stream-transient added
	if len(sel.reports) != 1 || sel.reports[0].Class != ClassSuccess {
		t.Fatalf("reports = %+v, want a single success (client disconnect must not penalize the endpoint)", sel.reports)
	}
}

// A cleanly completed stream still produces only one Report (success) and
// attempt = success — the counterpart to the previous test.
func TestDispatcher_CleanStreamSingleSuccessReport(t *testing.T) {
	sel := newFakeSelector(selResp{ep: newTestEP(1)})
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(sel),
		WithInvokerFactory(newFakeInvokerFactory(successResult(&domain.Usage{Total: 10}, 5))),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)
	out := d.Dispatch(context.Background(), httptest.NewRecorder(), newTestInput("gpt-4"))

	if len(sel.reports) != 1 || sel.reports[0].Class != ClassSuccess {
		t.Errorf("reports = %+v, want a single success", sel.reports)
	}
	if out.Decision.Attempts[0].Outcome != domain.AttemptSuccess {
		t.Errorf("attempt outcome = %s, want success", out.Decision.Attempts[0].Outcome)
	}
}

// Invoker.Invoke returns err (custom implementation path): must not Abort
// directly and bypass cooldown/retry — should Record + Report + go through
// RetryPolicy (transient → Continue → next ep).
func TestDispatcher_InvokeErrorGoesThroughRetryPath(t *testing.T) {
	sel := newFakeSelector(selResp{ep: newTestEP(1)}, selResp{ep: newTestEP(2)})
	broken := &fakeResult{invokeErr: errors.New("cannot build invocation")}
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(sel),
		WithInvokerFactory(newFakeInvokerFactory(broken, successResult(&domain.Usage{Total: 5}, 3))),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)
	out := d.Dispatch(context.Background(), httptest.NewRecorder(), newTestInput("gpt-4"))

	if out.Result != OutcomeStreamed {
		t.Fatalf("Result = %s, want Streamed (should retry to the second ep after invoke err)", out.Result)
	}
	if len(sel.reports) != 2 {
		t.Fatalf("reports = %d, want 2 (invoke-err transient + success)", len(sel.reports))
	}
	if sel.reports[0].Class != ClassTransient || sel.reports[0].Stage != StageInvoke {
		t.Errorf("first report = {%s, %s}, want {invoke, transient}", sel.reports[0].Stage, sel.reports[0].Class)
	}
}

// quotaVerdictToAttempt: dependency failures (qerr / ClassUnknown) stay Unknown
// (retry but no cooldown); genuine capacity rejections stay Capacity (retry +
// cooldown).
func TestQuotaVerdictToAttempt_ClassSemantics(t *testing.T) {
	// genuine rejection
	v := quotaVerdictToAttempt(&QuotaVerdict{Class: ClassCapacity, BucketKey: "rl:endpoint:1:rpm"}, nil)
	if v.Class != ClassCapacity {
		t.Errorf("genuine rejection class = %s, want capacity", v.Class)
	}
	// dependency failure (QuotaVerdict explicitly Unknown)
	v = quotaVerdictToAttempt(&QuotaVerdict{Class: ClassUnknown, Reason: "redis: connection refused"}, nil)
	if v.Class != ClassUnknown {
		t.Errorf("store error class = %s, want unknown (must not escalate to capacity and pollute cooldown)", v.Class)
	}
	// dependency failure (implementation returns err directly)
	v = quotaVerdictToAttempt(nil, errors.New("redis timeout"))
	if v.Class != ClassUnknown {
		t.Errorf("qerr class = %s, want unknown", v.Class)
	}
}

// cancelingInvokerFactory simulates a client that disconnects while the
// upstream call is in flight: Invoke cancels the request ctx, then returns
// the configured result (the way a canceled Do surfaces as a verdict).
type cancelingInvokerFactory struct {
	cancel context.CancelFunc
	res    *fakeResult
}

func (f *cancelingInvokerFactory) For(ep *domain.Endpoint, _ protocol.Handler, _ *domain.RequestEnvelope) Invoker {
	f.res.ep = ep
	return &cancelingInvoker{cancel: f.cancel, res: f.res}
}

type cancelingInvoker struct {
	cancel context.CancelFunc
	res    *fakeResult
}

func (i *cancelingInvoker) Invoke(_ context.Context) (Result, error) {
	i.cancel()
	return i.res, nil
}

// TestDispatcher_ClientGoneBeforeSelectAbortsImmediately: a ctx that is
// already canceled at Dispatch entry must not touch selector or invoker
// (both fakes panic when called) and must map to 499.
func TestDispatcher_ClientGoneBeforeSelectAbortsImmediately(t *testing.T) {
	sel := newFakeSelector() // Pick panics if reached
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(sel),
		WithInvokerFactory(newFakeInvokerFactory()), // For panics if reached
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := d.Dispatch(ctx, httptest.NewRecorder(), newTestInput("gpt-4"))

	if out.Result != OutcomeClientAbort {
		t.Fatalf("Result = %s, want client_abort", out.Result)
	}
	if out.HTTPCode != 499 {
		t.Fatalf("HTTPCode = %d, want 499", out.HTTPCode)
	}
	if len(sel.reports) != 0 {
		t.Fatalf("reports = %d, want 0 (client abort must not feed cooldown/stats)", len(sel.reports))
	}
}

// TestDispatcher_ClientGoneDuringInvokeNotReported: the ctx dies while the
// invoke is in flight. The resulting transient verdict is caused by the
// client, not the endpoint — it must not be Reported (no cooldown/stats
// pollution), must not be retried, and the P2C Release pairing must hold.
func TestDispatcher_ClientGoneDuringInvokeNotReported(t *testing.T) {
	ep := newTestEP(1)
	sel := newFakeSelector(selResp{ep: ep})
	res := transientResult()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(sel),
		WithInvokerFactory(&cancelingInvokerFactory{cancel: cancel, res: res}),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	out := d.Dispatch(ctx, httptest.NewRecorder(), newTestInput("gpt-4"))

	if out.Result != OutcomeClientAbort {
		t.Fatalf("Result = %s, want client_abort", out.Result)
	}
	if len(sel.reports) != 0 {
		t.Fatalf("reports = %d, want 0 (canceled ctx must not poison a healthy endpoint)", len(sel.reports))
	}
	if !res.closed {
		t.Fatalf("result not closed on client abort")
	}
	if sel.releases != 1 {
		t.Fatalf("releases = %d, want 1 (P2C pairing must survive the abort path)", sel.releases)
	}
}

// TestDispatcher_DeadlineExceededMapsTo504: a gateway-enforced timeout is not
// a client disconnect — it maps to 504 instead of 499.
func TestDispatcher_DeadlineExceededMapsTo504(t *testing.T) {
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector()),
		WithInvokerFactory(newFakeInvokerFactory()),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	out := d.Dispatch(ctx, httptest.NewRecorder(), newTestInput("gpt-4"))

	if out.Result != OutcomeClientAbort {
		t.Fatalf("Result = %s, want client_abort", out.Result)
	}
	if out.HTTPCode != 504 {
		t.Fatalf("HTTPCode = %d, want 504", out.HTTPCode)
	}
}
