// Package adapters 提供 dispatch.Dispatcher 各 port 的默认实现——把
// pkg/selector / pkg/invoker / pkg/ratelimit 等 primitive 能力组合成 dispatch
// 的端口。
//
// **依赖方向**：
//
//	pkg/dispatch/adapters → pkg/dispatch (ports)
//	pkg/dispatch/adapters → pkg/selector / pkg/invoker / pkg/ratelimit / pkg/protocol / pkg/moderation
//	pkg/dispatch         ✗ adapters（dispatch 不知道有 adapters）
//
// **不是 cmd/gateway 那种 wiring 文件**——这些 adapter 类型可被其它装配点
// （e.g. integration test、独立 binary）复用；wiring 只是装配 Dispatcher + 注入
// 这些 adapter。
//
// **三个 adapter**：
//
//	Selector       ── SelectorAdapter (CandidateSource + Scheduler + eligibility)
//	InvokerFactory ── InvokerFactoryAdapter (invoker.Sender)
//	EndpointQuota  ── EndpointQuotaAdapter (ratelimit.Store + bucket helpers)
package adapters

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/selector"
	"github.com/zereker/llm-gateway/pkg/selector/eligibility"
)

// SelectorAdapter 实现 dispatch.Selector——CandidateSource 拉候选 + eligibility
// 过滤 + selector.Scheduler.Pick；Report 把 dispatch.Verdict 翻成 selector.Result
// 灌进 Scheduler 的 cooldown 反馈通路。
type SelectorAdapter struct {
	candidates dispatch.CandidateSource
	sched      selector.Scheduler
}

// NewSelector 构造 SelectorAdapter；两个参数必填。
func NewSelector(candidates dispatch.CandidateSource, sched selector.Scheduler) *SelectorAdapter {
	return &SelectorAdapter{candidates: candidates, sched: sched}
}

// Select 实现 dispatch.Selector.Select。
func (s *SelectorAdapter) Select(ctx context.Context, q dispatch.Query) (*domain.Endpoint, error) {
	raw, err := s.candidates.ListForModel(ctx, q.Model, q.Identity.Group)
	if err != nil {
		return nil, err
	}
	elgRes := eligibility.Filter(raw, q.Envelope, q.Handlers)
	if len(elgRes.Eligible) == 0 {
		return nil, nil
	}
	cands := make([]selector.Candidate, len(elgRes.Eligible))
	for i, ep := range elgRes.Eligible {
		cands[i] = selector.Candidate{Endpoint: ep, EffectiveWeight: float64(ep.Weight)}
	}
	return s.sched.Pick(ctx, &selector.Request{
		Model:      q.Model,
		Group:      q.Identity.Group,
		Candidates: cands,
		ExcludeIDs: q.Exclude,
	})
}

// Report 实现 dispatch.Selector.Report——把 dispatch.Verdict 翻成 selector.Result
// 灌进 Scheduler 的 cooldown 反馈通路。
func (s *SelectorAdapter) Report(ctx context.Context, ep *domain.Endpoint, v dispatch.Verdict) {
	s.sched.Report(ctx, ep, selector.Result{
		Class:    dispatchClassToSelector(v.Class),
		HTTPCode: v.HTTPCode,
		Reason:   v.Reason,
		Latency:  v.Latency,
	})
}

// dispatchClassToSelector dispatch.Class → selector.ErrorClass（1:1 映射）。
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

// 编译期断言。
var _ dispatch.Selector = (*SelectorAdapter)(nil)
