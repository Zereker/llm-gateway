package adapters

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/selector"
)

// PickerAdapter 实现 dispatch.Selector——薄壳，把 selector.Scheduler 包成
// dispatch port。
//
// **职责严格 = 选 + 反馈**：
//   - Pick: 把 eligible 候选 + PickQuery 转成 selector.Request 调 Scheduler.Pick
//   - Report: 把 dispatch.Verdict 翻成 selector.Result 灌进 Scheduler 的 cooldown 反馈
//
// **不做**：候选拉取（CandidateSource 单独负责）、eligibility 过滤（dispatch
// 内部 filterEligible helper 完成）。这是 v0.6 把"选 endpoint"拆 3 步的体现。
type PickerAdapter struct {
	sched selector.Scheduler
}

// NewSelector 构造 PickerAdapter。
func NewSelector(sched selector.Scheduler) *PickerAdapter {
	return &PickerAdapter{sched: sched}
}

// Pick 实现 dispatch.Selector.Pick。
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

// Report 实现 dispatch.Selector.Report——把 dispatch.Verdict 翻成 selector.Result。
func (s *PickerAdapter) Report(ctx context.Context, ep *domain.Endpoint, v dispatch.Verdict) {
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
var _ dispatch.Selector = (*PickerAdapter)(nil)
