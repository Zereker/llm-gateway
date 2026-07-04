package dispatch

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/trace"
)

// captureTracer 把 StartSpan / SetAttribute / End / Log 全记录下来供断言用。
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
			selResp{ep: nil}, // primary 候选耗尽
			selResp{ep: ep},  // fallback 模型有候选
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

// TestDispatcher_InvokerDepFail: Invoker.Invoke 返 err → 按 transient 走 retry
// 流程（Record + Report + Continue），候选耗尽后 NoEndpoint 503。
// （旧行为是直接 Abort DepFail，绕过 cooldown / retry——review 修复后不再如此。）
func TestDispatcher_InvokerDepFail(t *testing.T) {
	r := &fakeResult{invokeErr: errFakeDep}
	sel := newFakeSelector(
		selResp{ep: newTestEP(1)},
		selResp{ep: nil}, // retry 后候选耗尽
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
		t.Fatalf("want OutcomeNoEndpoint（retry 后耗尽）, got %s (reason=%s)", out.Result, out.Reason)
	}
	if len(sel.reports) != 1 || sel.reports[0].Class != ClassTransient {
		t.Errorf("invoke err 应产生一条 transient Report，got %+v", sel.reports)
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

// TestDispatcher_TracerSpansHappyPath: WithTracer 注入时 happy path 生成
// dispatch.request + dispatch.attempt 两个 span，attrs 含 model / endpoint / verdict。
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

// TestDispatcher_DecisionAlwaysFilled_NoAttempts 验证 Outcome.Decision 契约：
// **即使一次 attempt 都没跑**（无 eligible / 无 candidate / cap=0），Decision 也
// 必须填出来，方便审计 / log / metric 不用对 nil 特判。
//
// 之前的 bug：state.finalize() 在 len(decisions)==0 时直接 return，留 nil Decision。
func TestDispatcher_DecisionAlwaysFilled_NoAttempts(t *testing.T) {
	d := New(
		WithCandidates(fakeCandidates{}),
		WithSelector(newFakeSelector(selResp{ep: nil})), // 直接 picker 返 nil
		WithInvokerFactory(newFakeInvokerFactory()),
		WithCap(HeaderAttemptCap{Default: 3}),
		WithRetry(DefaultRetry{}),
		WithFallback(ModelChainFallback{}),
	)

	in := newTestInput("gpt-4") // 单 model；fallback 也没；直接 NoEndpoint
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
		// 没成功路由时审计 routed 兜底成 primary，方便下游 join
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
// review 修复回归：dispatch 错误语义
// =============================================================================

// 200 之后流中断：Result 仍是 Streamed（状态码已写出），但 (1) selector 必须收到
// StageStream/transient 反馈（否则坏 endpoint 统计上 100% 成功），(2) 审计 attempt
// 不能标 success。
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
		t.Fatalf("Result = %s, want Streamed（状态码已写出不可回滚）", out.Result)
	}
	// Report 两条：invoke success + stream transient
	if len(sel.reports) != 2 {
		t.Fatalf("selector 收到 %d 条 Report，want 2（invoke + stream）", len(sel.reports))
	}
	if sel.reports[1].Stage != StageStream || sel.reports[1].Class != ClassTransient {
		t.Errorf("stream 失败反馈 = {%s, %s}, want {stream, transient}",
			sel.reports[1].Stage, sel.reports[1].Class)
	}
	// 审计 attempt 不标 success
	if got := out.Decision.Attempts[0].Outcome; got != domain.AttemptFail {
		t.Errorf("attempt outcome = %s, want fail（流没有完整交付）", got)
	}
}

// 客户端在流式途中主动断开（请求 ctx 取消）：状态码已写出，但 endpoint 是健康的——
// 不能因为客户端断连就补一条 stream-transient 惩罚 endpoint（打 cooldown），否则
// "客户端频繁取消"会误伤健康 endpoint。只保留 pre-stream 的 success Report。
func TestDispatcher_ClientAbortDoesNotPenalizeEndpoint(t *testing.T) {
	ep := newTestEP(1)
	sel := newFakeSelector(selResp{ep: ep})
	res := successResult(&domain.Usage{Total: 10}, 5)
	res.streamRep.Err = context.Canceled // 流中断，但源于客户端断开

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟客户端断开：请求 ctx 已取消

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
		t.Fatalf("Result = %s, want Streamed（状态码已写出）", out.Result)
	}
	// 只有 pre-stream success 一条；不补 stream-transient
	if len(sel.reports) != 1 || sel.reports[0].Class != ClassSuccess {
		t.Fatalf("reports = %+v, want 单条 success（客户端断开不惩罚 endpoint）", sel.reports)
	}
}

// 干净跑完的 stream 仍然只有一条 Report（success）且 attempt = success——
// 上一个测试的反向对照。
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
		t.Errorf("reports = %+v, want 单条 success", sel.reports)
	}
	if out.Decision.Attempts[0].Outcome != domain.AttemptSuccess {
		t.Errorf("attempt outcome = %s, want success", out.Decision.Attempts[0].Outcome)
	}
}

// Invoker.Invoke 返 err（自定义实现的路径）：不能直接 Abort 绕过 cooldown/retry——
// 应 Record + Report + 走 RetryPolicy（transient → Continue → 下一个 ep）。
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
		t.Fatalf("Result = %s, want Streamed（invoke err 后应 retry 到第二个 ep）", out.Result)
	}
	if len(sel.reports) != 2 {
		t.Fatalf("reports = %d 条, want 2（invoke-err transient + success）", len(sel.reports))
	}
	if sel.reports[0].Class != ClassTransient || sel.reports[0].Stage != StageInvoke {
		t.Errorf("第一条反馈 = {%s, %s}, want {invoke, transient}", sel.reports[0].Stage, sel.reports[0].Class)
	}
}

// quotaVerdictToAttempt：依赖故障（qerr / ClassUnknown）保持 Unknown（retry 但不
// cooldown）；真容量拒绝保持 Capacity（retry + cooldown）。
func TestQuotaVerdictToAttempt_ClassSemantics(t *testing.T) {
	// 真拒绝
	v := quotaVerdictToAttempt(&QuotaVerdict{Class: ClassCapacity, BucketKey: "rl:endpoint:1:rpm"}, nil)
	if v.Class != ClassCapacity {
		t.Errorf("真拒绝 class = %s, want capacity", v.Class)
	}
	// 依赖故障（QuotaVerdict 显式 Unknown）
	v = quotaVerdictToAttempt(&QuotaVerdict{Class: ClassUnknown, Reason: "redis: connection refused"}, nil)
	if v.Class != ClassUnknown {
		t.Errorf("store 错误 class = %s, want unknown（不能升级成 capacity 污染 cooldown）", v.Class)
	}
	// 依赖故障（实现直接返 err）
	v = quotaVerdictToAttempt(nil, errors.New("redis timeout"))
	if v.Class != ClassUnknown {
		t.Errorf("qerr class = %s, want unknown", v.Class)
	}
}
