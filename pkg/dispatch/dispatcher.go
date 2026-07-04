package dispatch

import (
	"context"
	"net/http"
	"strconv"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/trace"
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
	candidates     CandidateSource
	selector       Selector
	invokerFactory InvokerFactory
	quota          EndpointQuota
	cap            AttemptCap
	retry          RetryPolicy
	fallback       FallbackPolicy
	tracer         trace.Tracer // 可选；nil → 不开 span（与 SlogTracer NoOp 等价）
}

// New 装配一个 Dispatcher。
//
// **必填**：CandidateSource / Selector / InvokerFactory / AttemptCap / RetryPolicy /
// FallbackPolicy 任一缺失 → panic。fail-fast 暴露配置错。
//
// **可选**：EndpointQuota（不传 = NoopQuota 永不拒绝）。
func New(opts ...Option) *Dispatcher {
	d := &Dispatcher{}
	for _, opt := range opts {
		opt(d)
	}
	if d.candidates == nil {
		panic("dispatch.New: WithCandidates required")
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
	if d.tracer == nil {
		d.tracer = trace.NewSlogTracer(nil) // NoOp span，hot path 零开销
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

	ctx, span := d.tracer.StartSpan(ctx, "dispatch.request")
	span.SetAttribute("dispatch.model", s.CurrentModelName())
	span.SetAttribute("dispatch.group", s.Group())
	span.SetAttribute("dispatch.attempt_cap", s.AttemptsCap())
	defer span.End()

	for {
		switch a := d.step(ctx, w, s).(type) {
		case Continue:
			// 同 model 再选一个；Record 已 exclude，直接进下一轮 Select
		case Switch:
			prev := s.CurrentModelName()
			s.SetModel(a.Next)
			d.tracer.Log(ctx, "dispatch.fallback", map[string]string{
				"from": prev, "to": modelName(a.Next),
			})
		case Stream:
			// step 内部已完成 ApplyStream；Stream{} 仅作"已处理"信号
			out := s.Outcome()
			span.SetAttribute("dispatch.outcome", out.Result.String())
			span.SetAttribute("dispatch.routed_model", modelName(out.RoutedModel))
			span.SetAttribute("dispatch.attempts", s.Attempts())
			return out
		case Abort:
			s.SetAbort(a)
			out := s.Outcome()
			span.SetAttribute("dispatch.outcome", out.Result.String())
			span.SetAttribute("dispatch.http_code", a.HTTPCode)
			span.SetAttribute("dispatch.attempts", s.Attempts())
			return out
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

	ctx, span := d.tracer.StartSpan(ctx, "dispatch.attempt")
	span.SetAttribute("attempt.model", s.CurrentModelName())
	span.SetAttribute("attempt.index", s.Attempts())
	defer span.End()

	// === CandidateSource → filter → Selector.Pick：三个步骤分立 ===
	candidates, err := d.candidates.ListForModel(ctx, s.CurrentModelName(), s.Group())
	if err != nil {
		return Abort{
			Result:   OutcomeDepFail,
			Class:    ClassTransient,
			HTTPCode: 503,
			Reason:   "candidates: " + err.Error(),
		}
	}
	span.SetAttribute("attempt.candidates", len(candidates))
	eligible := filterEligible(candidates, s.Envelope(), s.Handlers())
	span.SetAttribute("attempt.eligible", len(eligible))
	if len(eligible) == 0 {
		span.SetAttribute("attempt.exit", "no_eligible")
		return d.fallback.OnExhausted(s)
	}
	ep, err := d.selector.Pick(ctx, eligible, s.PickQuery())
	if err != nil {
		return Abort{
			Result:   OutcomeDepFail,
			Class:    ClassTransient,
			HTTPCode: 503,
			Reason:   "select: " + err.Error(),
		}
	}
	if ep == nil {
		// 候选有 eligible，但 picker 因 cooldown 等原因全 skip——交给 FallbackPolicy
		span.SetAttribute("attempt.exit", "picker_skipped_all")
		return d.fallback.OnExhausted(s)
	}
	annotateEndpoint(span, ep)

	// === EndpointQuota.Reserve（前扣）===
	if denied, qerr := d.quota.Reserve(ctx, ep); denied != nil || qerr != nil {
		v := quotaVerdictToAttempt(denied, qerr)
		annotateVerdict(span, v)
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
		annotateVerdict(span, v)
		s.Record(ep, v)
		d.selector.Report(ctx, ep, v)
		return d.retry.Decide(s, v)
	}

	// === Invoker.Invoke（纯 HTTP）===
	inv := d.invokerFactory.For(ep, handler, s.Envelope())
	res, ierr := inv.Invoke(ctx)
	if ierr != nil {
		// "无法构造调用"极少见（当前默认 InvokerFactory 不会走到；给自定义
		// Invoker 实现留的路径）。跟其它失败路径一致：Record + Report + Policy
		// 决策——直接 Abort 会绕过 cooldown / retry，把 transient 错放大成 503。
		v := Verdict{
			Stage:  StageInvoke,
			Class:  ClassTransient,
			Reason: "invoke: " + ierr.Error(),
		}
		annotateVerdict(span, v)
		s.Record(ep, v)
		d.selector.Report(ctx, ep, v)
		return d.retry.Decide(s, v)
	}
	defer res.Close()

	verdict := res.Verdict()
	annotateVerdict(span, verdict)
	s.Record(ep, verdict)
	d.selector.Report(ctx, ep, verdict)

	action := d.retry.Decide(s, verdict)
	if _, ok := action.(Stream); ok {
		// 成功路径——在 step 内消费 res（资源生命周期不能跨方法）
		rep := res.StreamTo(ctx, w)
		s.ApplyStream(rep)
		// === EndpointQuota.ChargeUsage（后扣，fire-and-forget）===
		d.quota.ChargeUsage(ctx, ep, rep.Usage)
		if rep.Err != nil {
			// 200 之后流中断：状态码已写出，不能 retry，但必须让 cooldown /
			// stats 看到——否则"200 后 RST"的坏 endpoint 统计上永远 100% 成功，
			// 流量持续打过去。pre-stream 的 Success Report 已经发过，这里补一条
			// StageStream 的 transient 失败覆盖它。
			sv := Verdict{
				Stage:  StageStream,
				Class:  ClassTransient,
				Reason: "stream: " + rep.Err.Error(),
			}
			span.SetAttribute("stream.err", rep.Err.Error())
			d.selector.Report(ctx, ep, sv)
		}
		return Stream{}
	}
	return action
}

// modelName 安全取 *domain.ModelService 的 Model（防 nil）。
func modelName(m *domain.ModelService) string {
	if m == nil {
		return ""
	}
	return m.Model
}

// annotateEndpoint 给当前 span 打 endpoint 选中后的标签。
func annotateEndpoint(span trace.Span, ep *domain.Endpoint) {
	if ep == nil {
		return
	}
	span.SetAttribute("endpoint.id", strconv.FormatInt(ep.ID, 10))
	span.SetAttribute("endpoint.vendor", ep.Vendor)
	span.SetAttribute("endpoint.protocol", ep.Protocol.String())
}

// annotateVerdict 给 span 打 attempt 结果。
func annotateVerdict(span trace.Span, v Verdict) {
	span.SetAttribute("verdict.stage", v.Stage.String())
	span.SetAttribute("verdict.class", v.Class.String())
	if v.HTTPCode != 0 {
		span.SetAttribute("verdict.http_code", v.HTTPCode)
	}
	if v.Reason != "" {
		span.SetAttribute("verdict.reason", v.Reason)
	}
}

// quotaVerdictToAttempt 把 EndpointQuota.Reserve 的拒绝结果（QuotaVerdict）翻成
// dispatch.Verdict（attempt-level 报告，用于 retry / Selector.Report）。
//
// **Class 语义**（docs/04 §8）：
//   - ClassCapacity ── 真配额拒绝：retry 换 ep + 该 ep 写 capacity cooldown
//   - ClassUnknown  ── 依赖故障（Redis 错等）：retry 换 ep，但 **不写 cooldown**
//     ——不能把"Redis 抖动"误标成"endpoint 坏了"，否则一次抖动把路径上每个
//     健康 endpoint 都打进冷却，恢复后污染还残留一个 TTL
//
// denied.Class 由 EndpointQuota 实现显式填；denied == nil 但 qerr != nil
// （实现直接返错）同样按依赖故障（Unknown）处理。
func quotaVerdictToAttempt(denied *QuotaVerdict, qerr error) Verdict {
	if denied != nil {
		reason := denied.Reason
		if denied.BucketKey != "" && reason == "" {
			reason = "endpoint quota exhausted: " + denied.BucketKey
		}
		return Verdict{Stage: StageReserve, Class: denied.Class, Reason: reason}
	}
	return Verdict{
		Stage:  StageReserve,
		Class:  ClassUnknown,
		Reason: "endpoint quota (store error): " + qerr.Error(),
	}
}
