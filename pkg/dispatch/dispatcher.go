package dispatch

import (
	"context"
	"net/http"
)

// Dispatcher 协调 Selector + Invoker + Policy，把一次请求路由到合适的 endpoint
// 并执行。
//
// **设计精神**：Dispatcher 自身只跑 Action reducer 循环，业务真相分布在三个
// Policy 实现里。新增 retry 策略 / fallback 策略不动 Dispatcher，写新 Policy
// 注入即可。
//
// **生命周期**：单实例（startup wiring），并发安全（无 per-request state；
// state 是每请求 new 出来的）。
type Dispatcher struct {
	selector       Selector
	invokerFactory InvokerFactory
	quota          EndpointQuota
	cap            AttemptCap
	retry          RetryPolicy
	fallback       FallbackPolicy
}

// New 装配一个 Dispatcher。
//
// **必填**：Selector / InvokerFactory / AttemptCap / RetryPolicy / FallbackPolicy
// 任一缺失 → panic。fail-fast 暴露配置错。
//
// **可选**：EndpointQuota（不传 = NoopQuota 永不拒绝）。
func New(opts ...Option) *Dispatcher {
	d := &Dispatcher{}
	for _, opt := range opts {
		opt(d)
	}
	if d.selector == nil {
		panic("dispatch.New: WithSelector required")
	}
	if d.invokerFactory == nil {
		panic("dispatch.New: WithInvokerFactory required")
	}
	if d.cap == nil {
		panic("dispatch.New: WithCap required")
	}
	if d.retry == nil {
		panic("dispatch.New: WithRetry required")
	}
	if d.fallback == nil {
		panic("dispatch.New: WithFallback required")
	}
	if d.quota == nil {
		d.quota = NoopQuota{}
	}
	return d
}

// Dispatch 入口。framework-free——只认 stdlib http.ResponseWriter 和 typed Input。
//
// **流程**：
//
//	state := newState(in, cap.Resolve(in))
//	for {
//	    action := d.step(ctx, w, state)
//	    switch action.(type) { Continue / Switch / Stream / Abort }
//	}
//
// **返回**：Outcome.Result == OutcomeStreamed 表示响应已通过 w 写出；
// 否则 middleware 需根据 HTTPCode / Class / Reason 写错误响应。caller 把
// outcome.Decision / Usage / RoutedModel / Error 等字段映射回 RC（dispatch 不直接动 RC）。
func (d *Dispatcher) Dispatch(ctx context.Context, w http.ResponseWriter, in Input) Outcome {
	s := newState(in, d.cap.Resolve(in))

	for {
		switch a := d.step(ctx, w, s).(type) {
		case Continue:
			// 同 model 再选一个；Record 已 exclude，直接进下一轮 Select
		case Switch:
			s.SetModel(a.Next)
		case Stream:
			// step 内部已完成 ApplyStream；Stream{} 仅作"已处理"信号
			return s.Outcome()
		case Abort:
			s.SetAbort(a)
			return s.Outcome()
		}
	}
}

// step 跑业务的一次循环：select → quota.Reserve → handler 查找 → invoke →
// selector.Report → policy 决策；stream 成功时还会 quota.Charge。
//
// **特殊**：Stream 决策时直接在 step 内部完成 StreamTo + ApplyStream
// （res 资源在 step 栈内 defer Close，不能跨方法返回）；step 返回 Stream{}
// 仅作为信号让 Dispatch 退出循环。
func (d *Dispatcher) step(ctx context.Context, w http.ResponseWriter, s *state) Action {
	if s.Exhausted() {
		return Abort{
			Result:   OutcomeNoEndpoint,
			Class:    ClassUnknown,
			HTTPCode: 503,
			Reason:   "attempts exhausted",
		}
	}

	ep, err := d.selector.Select(ctx, s.Query())
	if err != nil {
		return Abort{
			Result:   OutcomeDepFail,
			Class:    ClassTransient,
			HTTPCode: 503,
			Reason:   "select: " + err.Error(),
		}
	}
	if ep == nil {
		// 当前 model 候选耗尽——交给 FallbackPolicy
		return d.fallback.OnExhausted(s)
	}

	// === EndpointQuota.Reserve（前扣）===
	if denied, qerr := d.quota.Reserve(ctx, ep); denied != nil || qerr != nil {
		v := quotaVerdictToAttempt(denied, qerr)
		s.Record(ep, v)
		d.selector.Report(ctx, ep, v)
		return d.retry.Decide(s, v)
	}

	// === Handler 查找 ===
	// 按 (endpoint, srcProto) 动态组合 Handler。
	// eligibility filter 已挡掉 handler-missing 的 endpoint，这里再防一手。
	handler := s.Handlers().Get(ep, s.in.SourceProtocol())
	if handler == nil {
		v := Verdict{
			Stage:    StagePrepare,
			Class:    ClassPermanent,
			HTTPCode: 502,
			Reason:   "no handler for endpoint+srcProto",
		}
		s.Record(ep, v)
		d.selector.Report(ctx, ep, v)
		return d.retry.Decide(s, v)
	}

	// === Invoker.Invoke（纯 HTTP）===
	inv := d.invokerFactory.For(ep, handler, s.Envelope())
	res, ierr := inv.Invoke(ctx)
	if ierr != nil {
		return Abort{
			Result:   OutcomeDepFail,
			Class:    ClassTransient,
			HTTPCode: 503,
			Reason:   "invoke: " + ierr.Error(),
		}
	}
	defer res.Close()

	verdict := res.Verdict()
	s.Record(ep, verdict)
	d.selector.Report(ctx, ep, verdict)

	action := d.retry.Decide(s, verdict)
	if _, ok := action.(Stream); ok {
		// 成功路径——在 step 内消费 res（资源生命周期不能跨方法）
		rep := res.StreamTo(ctx, w)
		s.ApplyStream(rep)
		// === EndpointQuota.ChargeUsage（后扣，fire-and-forget）===
		d.quota.ChargeUsage(ctx, ep, rep.Usage)
		return Stream{}
	}
	return action
}

// quotaVerdictToAttempt 把 EndpointQuota.Reserve 的拒绝结果（QuotaVerdict）翻成
// dispatch.Verdict（attempt-level 报告，用于 retry / Selector.Report）。
// quota 返 nil verdict 但有 err（依赖故障）时也按 capacity 处理（让 retry 换 ep）。
func quotaVerdictToAttempt(denied *QuotaVerdict, qerr error) Verdict {
	if denied != nil {
		class := denied.Class
		if class == ClassUnknown {
			class = ClassCapacity
		}
		reason := denied.Reason
		if denied.BucketKey != "" && reason == "" {
			reason = "endpoint quota exhausted: " + denied.BucketKey
		}
		return Verdict{Stage: StageReserve, Class: class, Reason: reason}
	}
	return Verdict{
		Stage:  StageReserve,
		Class:  ClassCapacity,
		Reason: "endpoint quota: " + qerr.Error(),
	}
}
