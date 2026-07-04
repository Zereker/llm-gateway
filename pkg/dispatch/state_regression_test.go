package dispatch

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// TestState_RecordSkipsExcludeOnClassUnknown 单元级回归：ClassUnknown（依赖故障 /
// 分类盲区）不能把 endpoint 加进 excluded——否则一次 Redis 抖动会把健康 endpoint
// 从后续候选池永久删掉。其它 class 仍然正常 exclude。
func TestState_RecordSkipsExcludeOnClassUnknown(t *testing.T) {
	s := newState(newTestInput("gpt-4"), 5)
	ep := newTestEP(7)

	s.Record(ep, Verdict{Stage: StageReserve, Class: ClassUnknown, Reason: "redis blip"})
	if _, ok := s.Excluded()[ep.ID]; ok {
		t.Fatal("ClassUnknown 不应 exclude endpoint（依赖故障非 endpoint 之过）")
	}
	if s.Attempts() != 1 {
		t.Fatalf("attempts = %d, want 1（unknown 仍占一次 attempt）", s.Attempts())
	}

	// 反向对照：真失败（transient）必须 exclude。
	s.Record(ep, Verdict{Stage: StageInvoke, Class: ClassTransient})
	if _, ok := s.Excluded()[ep.ID]; !ok {
		t.Fatal("ClassTransient 必须 exclude endpoint")
	}
}

// excludeAwareSelector 尊重 PickQuery.Exclude：被 exclude 的 endpoint 不再返回
// （fakeSelector 忽略 exclude，无法验证 excluded 集合对候选的真实影响）。
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

// singleEPCandidates 永远只返回同一个 endpoint（跨 model 共享的场景）。
type singleEPCandidates struct{ ep *domain.Endpoint }

func (c singleEPCandidates) ListForModel(_ context.Context, _, _ string) ([]*domain.Endpoint, error) {
	return []*domain.Endpoint{c.ep}, nil
}

// unknownThenOKQuota：第一次 Reserve 返回依赖故障（store err → ClassUnknown），
// 之后放行——模拟一次 Redis 抖动。
type unknownThenOKQuota struct{ calls int }

func (q *unknownThenOKQuota) Reserve(_ context.Context, _ *domain.Endpoint) (*QuotaVerdict, error) {
	q.calls++
	if q.calls == 1 {
		return nil, errors.New("redis: connection reset")
	}
	return nil, nil
}

func (q *unknownThenOKQuota) ChargeUsage(_ context.Context, _ *domain.Endpoint, _ *domain.Usage) {}

// TestDispatcher_ClassUnknownEndpointStaysEligible 端到端回归：唯一 endpoint 撞了
// 一次 store 抖动（ClassUnknown），必须仍留在候选池里被下一次 attempt 重选，而不是
// 被 excluded 永久删掉后对健康 endpoint 报 503。
//
// 修复前：Record 无条件 exclude → 第二次 Pick 命中 exclude 返回 nil → 无候选 →
// OnExhausted → 单 model 无 fallback → 503。
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
		t.Fatalf("Result = %s, want Streamed（endpoint 撞抖动后应被重选成功）", out.Result)
	}
	if len(out.Decision.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2（unknown reserve + 成功 invoke）", len(out.Decision.Attempts))
	}
	if len(sel.reports) != 2 {
		t.Fatalf("reports = %d, want 2（unknown reserve + success invoke）", len(sel.reports))
	}
	if sel.reports[0].Class != ClassUnknown {
		t.Errorf("reports[0].Class = %s, want unknown（store 抖动不写 cooldown）", sel.reports[0].Class)
	}
}
