package selector

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/selector/eligibility"
)

// DispatchSelector 实现 dispatch.Selector——把 EndpointReader + Scheduler
// 包成 dispatch port。
//
// **职责**：
//   1. ListForModel 拉候选
//   2. eligibility.Filter（按 q.Handlers 一次性过滤 modality / adapter / translator）
//   3. 构造 Candidate（EffectiveWeight = ep.Weight）
//   4. Scheduler.Pick（filter chain → scorer → 内部 picker）
//
// **没 Report 方法**——Report 内化到 Invoker（DispatchInvokerFactory 在 Invoke
// 完成时调）。
//
// **归属在 pkg/selector**：这是"选 endpoint"的具体编排实现，跟 selector 自家
// 的 Scheduler / EndpointReader / eligibility 都耦合，自然归位。cmd/gateway 只做
// new + 注入。
type DispatchSelector struct {
	endpoints EndpointReader
	sched     Scheduler
}

// NewDispatchSelector 构造 DispatchSelector；endpoints / sched 必填。
func NewDispatchSelector(endpoints EndpointReader, sched Scheduler) *DispatchSelector {
	return &DispatchSelector{endpoints: endpoints, sched: sched}
}

// Select 实现 dispatch.Selector.Select。
func (s *DispatchSelector) Select(ctx context.Context, q dispatch.Query) (*domain.Endpoint, error) {
	raw, err := s.endpoints.ListForModel(ctx, q.Model, q.Identity.Group)
	if err != nil {
		return nil, err
	}
	elgRes := eligibility.Filter(raw, q.Envelope, q.Handlers)
	if len(elgRes.Eligible) == 0 {
		return nil, nil
	}
	cands := make([]Candidate, len(elgRes.Eligible))
	for i, ep := range elgRes.Eligible {
		cands[i] = Candidate{Endpoint: ep, EffectiveWeight: float64(ep.Weight)}
	}
	return s.sched.Pick(ctx, &Request{
		Model:      q.Model,
		Group:      q.Identity.Group,
		Candidates: cands,
		ExcludeIDs: q.Exclude,
	})
}

// Report 实现 dispatch.Selector.Report——把 dispatch.Verdict 翻成 selector.Result
// 灌进 Scheduler 的 cooldown 反馈通路。
func (s *DispatchSelector) Report(ctx context.Context, ep *domain.Endpoint, v dispatch.Verdict) {
	s.sched.Report(ctx, ep, Result{
		Class:    dispatchClassToSelector(v.Class),
		HTTPCode: v.HTTPCode,
		Reason:   v.Reason,
		Latency:  v.Latency,
	})
}

// dispatchClassToSelector dispatch.Class → selector.ErrorClass（1:1 映射）。
func dispatchClassToSelector(c dispatch.Class) ErrorClass {
	switch c {
	case dispatch.ClassSuccess:
		return ClassSuccess
	case dispatch.ClassTransient:
		return ClassTransient
	case dispatch.ClassCapacity:
		return ClassCapacity
	case dispatch.ClassPermanent:
		return ClassPermanent
	case dispatch.ClassInvalid:
		return ClassInvalid
	default:
		return ClassUnknown
	}
}

// 编译期断言。
var _ dispatch.Selector = (*DispatchSelector)(nil)
