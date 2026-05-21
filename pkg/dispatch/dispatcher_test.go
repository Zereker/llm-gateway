package dispatch

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// =============================================================================
// Dispatcher 端到端行为测试
// =============================================================================

// TestDispatcher_HappyPath: 首次 select 成功，verdict success → Stream。
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

// TestDispatcher_InvalidAbortsImmediately: invalid → 400，无重试。
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

// TestDispatcher_AttemptsExhausted: 一直 transient 到 cap → NoEndpoint 503。
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

// TestDispatcher_FallbackToNextModel: primary 候选耗尽 → switch fallback → success。
func TestDispatcher_FallbackToNextModel(t *testing.T) {
	ep := newTestEP(10)
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector(
			selResp{ep: nil},   // primary 候选耗尽
			selResp{ep: ep},    // fallback 模型有候选
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

// TestDispatcher_AllModelsExhausted: 所有 model 候选都耗尽 → NoEndpoint 503。
func TestDispatcher_AllModelsExhausted(t *testing.T) {
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector(
			selResp{ep: nil},
			selResp{ep: nil},
		)),
		WithInvokerFactory(newFakeInvokerFactory()), // 不会被调用
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

// TestDispatcher_SelectorDepFail: Selector.Select 返 err → DepFail 503。
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

// TestDispatcher_InvokerDepFail: Invoker.Invoke 返 err → DepFail 503。
func TestDispatcher_InvokerDepFail(t *testing.T) {
	r := &fakeResult{invokeErr: errFakeDep}
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector(selResp{ep: newTestEP(1)})),
		WithInvokerFactory(newFakeInvokerFactory(r)),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	in := newTestInput("gpt-4")
	out := d.Dispatch(context.Background(), httptest.NewRecorder(), in)

	if out.Result != OutcomeDepFail {
		t.Fatalf("want OutcomeDepFail, got %s (reason=%s)", out.Result, out.Reason)
	}
}

// TestDispatcher_TerminalNonRetryable: 永久错被 DefaultRetry 当 retryable 处理（换 ep 继续）。
//
// 注意：在 DefaultRetry 的语义下，permanent 是 retryable（换 ep 可能成功）；
// 只有 invalid 才直接 abort。所以这个 case 实际上会重试，直到 attempts 用完。
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

// TestDispatcher_PanicsOnMissingDeps: New() 缺依赖应 panic。
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
