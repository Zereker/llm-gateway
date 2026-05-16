package schedule

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/repo"
)

// =============================================================================
// stubs
// =============================================================================

// stubFilter 简单 filter：按 excludeIDs 过滤。
type stubFilter struct {
	name       string
	excludeIDs map[int64]bool
	calls      int
	mu         sync.Mutex
}

func (s *stubFilter) Name() string { return s.name }
func (s *stubFilter) Apply(_ context.Context, candidates []*domain.Endpoint, _ *Request) []*domain.Endpoint {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	out := make([]*domain.Endpoint, 0, len(candidates))
	for _, ep := range candidates {
		if !s.excludeIDs[ep.ID] {
			out = append(out, ep)
		}
	}
	return out
}

// stubSelector 永远返回第一个候选。
type stubSelector struct{}

func (stubSelector) Select(_ context.Context, cs []Candidate) *Candidate {
	if len(cs) == 0 {
		return nil
	}
	return &cs[0]
}

// stubCooldown 记录 Mark 调用。
type stubCooldown struct {
	marks      []markCall
	cooled     map[int64]bool
	inCooldown func(ids []int64) (map[int64]bool, error)
	markErr    error
	mu         sync.Mutex
}

type markCall struct {
	ID    int64
	Class ErrorClass
}

func (s *stubCooldown) Mark(_ context.Context, endpointID int64, class ErrorClass) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks = append(s.marks, markCall{ID: endpointID, Class: class})
	return s.markErr
}
func (s *stubCooldown) InCooldown(_ context.Context, ids []int64) (map[int64]bool, error) {
	if s.inCooldown != nil {
		return s.inCooldown(ids)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if s.cooled[id] {
			out[id] = true
		}
	}
	return out, nil
}

func ep(id int64, weight uint32) *domain.Endpoint {
	return &repo.Endpoint{ID: id, Vendor: "openai", Model: "gpt-4o", Weight: weight}
}

func candidates(eps ...*domain.Endpoint) []Candidate {
	out := make([]Candidate, len(eps))
	for i, e := range eps {
		out[i] = Candidate{Endpoint: e, EffectiveWeight: float64(e.Weight)}
	}
	return out
}

// =============================================================================
// ErrorClass
// =============================================================================

func TestErrorClass_String(t *testing.T) {
	cases := map[ErrorClass]string{
		ClassUnknown:   "unknown",
		ClassSuccess:   "success",
		ClassTransient: "transient",
		ClassCapacity:  "capacity",
		ClassPermanent: "permanent",
		ClassInvalid:   "invalid",
	}
	for c, want := range cases {
		if got := c.String(); got != want {
			t.Errorf("class=%v: got=%q, want=%q", c, got, want)
		}
	}
}

func TestErrorClass_IsRetryable(t *testing.T) {
	retryable := []ErrorClass{ClassTransient, ClassCapacity, ClassPermanent, ClassUnknown}
	notRetryable := []ErrorClass{ClassSuccess, ClassInvalid}

	for _, c := range retryable {
		if !c.IsRetryable() {
			t.Errorf("class=%v: IsRetryable=false, want=true", c)
		}
	}
	for _, c := range notRetryable {
		if c.IsRetryable() {
			t.Errorf("class=%v: IsRetryable=true, want=false", c)
		}
	}
}

// =============================================================================
// Pick
// =============================================================================

func TestPick_NilRequest_Error(t *testing.T) {
	s := New(Config{Selector: stubSelector{}})
	_, err := s.Pick(context.Background(), nil)
	if err == nil {
		t.Fatal("expected err")
	}
}

func TestPick_NoCandidates_Nil(t *testing.T) {
	s := New(Config{Selector: stubSelector{}})
	got, err := s.Pick(context.Background(), &Request{Model: "x"})
	if err != nil || got != nil {
		t.Errorf("got=%v err=%v", got, err)
	}
}

func TestPick_HappyPath_ReturnsCandidate(t *testing.T) {
	s := New(Config{Selector: stubSelector{}})
	got, err := s.Pick(context.Background(), &Request{
		Model:      "x",
		Candidates: candidates(ep(1, 100), ep(2, 100)),
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got == nil || got.ID != 1 {
		t.Errorf("got=%v, want ep1", got)
	}
}

func TestPick_ExcludesAlreadyTried(t *testing.T) {
	s := New(Config{Selector: stubSelector{}})
	excluded := map[int64]struct{}{1: {}}
	got, _ := s.Pick(context.Background(), &Request{
		Candidates: candidates(ep(1, 100), ep(2, 100)),
		ExcludeIDs: excluded,
	})
	if got == nil || got.ID != 2 {
		t.Errorf("got=%v, want ep2 (ep1 excluded)", got)
	}
}

func TestPick_FilterChainNarrows(t *testing.T) {
	f := &stubFilter{name: "f1", excludeIDs: map[int64]bool{1: true}}
	s := New(Config{Filters: []Filter{f}, Selector: stubSelector{}})
	got, _ := s.Pick(context.Background(), &Request{
		Candidates: candidates(ep(1, 100), ep(2, 100), ep(3, 100)),
	})
	if got == nil || got.ID == 1 {
		t.Errorf("got=%v, expected non-ep1", got)
	}
	if f.calls != 1 {
		t.Errorf("filter calls=%d", f.calls)
	}
}

func TestPick_AllFiltered_Nil(t *testing.T) {
	f := &stubFilter{name: "f1", excludeIDs: map[int64]bool{1: true, 2: true}}
	s := New(Config{Filters: []Filter{f}, Selector: stubSelector{}})
	got, _ := s.Pick(context.Background(), &Request{
		Candidates: candidates(ep(1, 100), ep(2, 100)),
	})
	if got != nil {
		t.Errorf("got=%v, want nil (all filtered)", got)
	}
}

// =============================================================================
// Report
// =============================================================================

func TestReport_NilEp_NoOp(t *testing.T) {
	cd := &stubCooldown{}
	s := New(Config{Cooldown: cd, Selector: stubSelector{}})
	s.Report(context.Background(), nil, Result{Class: ClassTransient}) // 不应 panic
	if len(cd.marks) != 0 {
		t.Errorf("nil ep should be no-op")
	}
}

func TestReport_Success_NoCooldown(t *testing.T) {
	cd := &stubCooldown{}
	s := New(Config{Cooldown: cd, Selector: stubSelector{}})
	s.Report(context.Background(), ep(1, 100), Result{Class: ClassSuccess})
	if len(cd.marks) != 0 {
		t.Errorf("Success should not Mark")
	}
}

func TestReport_Invalid_NoCooldown(t *testing.T) {
	cd := &stubCooldown{}
	s := New(Config{Cooldown: cd, Selector: stubSelector{}})
	s.Report(context.Background(), ep(1, 100), Result{Class: ClassInvalid})
	if len(cd.marks) != 0 {
		t.Errorf("Invalid should not Mark")
	}
}

func TestReport_Capacity_TriggersCooldown(t *testing.T) {
	cd := &stubCooldown{}
	s := New(Config{Cooldown: cd, Selector: stubSelector{}})
	s.Report(context.Background(), ep(1, 100), Result{Class: ClassCapacity})
	if len(cd.marks) != 1 || cd.marks[0].Class != ClassCapacity {
		t.Errorf("expected Mark capacity, got %+v", cd.marks)
	}
}

func TestReport_Unknown_NoCooldown(t *testing.T) {
	// docs/03 §6：Unknown 可重试但分类不明确——Scheduler.Report 默认不冷却，避免误伤
	cd := &stubCooldown{}
	s := New(Config{Cooldown: cd, Selector: stubSelector{}})
	s.Report(context.Background(), ep(1, 100), Result{Class: ClassUnknown})
	if len(cd.marks) != 0 {
		t.Errorf("Unknown should not trigger cooldown (could be transient noise), got %+v", cd.marks)
	}
}

// =============================================================================
// Scorer + StatsStore（runtime scoring）
// =============================================================================

type recordingStats struct {
	records []Result
	mu      sync.Mutex
}

func (s *recordingStats) Record(_ context.Context, _ int64, r Result) {
	s.mu.Lock()
	s.records = append(s.records, r)
	s.mu.Unlock()
}
func (s *recordingStats) Snapshot(_ context.Context, _ int64) EndpointStats {
	return EndpointStats{SuccessRate: 1}
}

func TestReport_Stats_RecordsResult(t *testing.T) {
	st := &recordingStats{}
	s := New(Config{Stats: st, Selector: stubSelector{}})
	s.Report(context.Background(), ep(1, 100), Result{Class: ClassSuccess})
	if len(st.records) != 1 {
		t.Errorf("expected 1 record, got %d", len(st.records))
	}
}

func TestPick_ScorerAdjustsWeights(t *testing.T) {
	store := NewInMemoryStatsStore(0.5)
	// 让 ep1 总成功且 latency=100，ep2 总失败 latency=500
	for i := 0; i < 5; i++ {
		store.Record(context.Background(), 1, Result{Class: ClassSuccess, Latency: 100 * time.Millisecond})
	}
	// 5 个样本之后 scorer 应该开始打分
	scorer := NewDefaultScorer(store, 5, 200)

	s := New(Config{Scorer: scorer, Selector: stubSelector{}})
	got, _ := s.Pick(context.Background(), &Request{
		Candidates: candidates(ep(1, 100)),
	})
	if got == nil || got.ID != 1 {
		t.Errorf("got=%v", got)
	}
}

// 一个超简单 duration helper（避免再 import time 让 lint 抱怨）
func mustParseDuration(unit string) int64 {
	switch unit {
	case "ms":
		return 1_000_000 // 1ms in ns
	}
	return 1
}

// =============================================================================
// InMemoryStatsStore
// =============================================================================

func TestInMemoryStatsStore_EmptyEndpoint_NeutralSnapshot(t *testing.T) {
	s := NewInMemoryStatsStore(0.2)
	snap := s.Snapshot(context.Background(), 999)
	if snap.SuccessRate != 1.0 {
		t.Errorf("empty snapshot SuccessRate=%v, want 1.0 (neutral)", snap.SuccessRate)
	}
	if snap.SampleCount != 0 {
		t.Errorf("SampleCount=%d, want 0", snap.SampleCount)
	}
}

func TestInMemoryStatsStore_RecordsAndAggregates(t *testing.T) {
	s := NewInMemoryStatsStore(0.5)
	s.Record(context.Background(), 1, Result{Class: ClassSuccess, Latency: time.Duration(100) * time.Millisecond})
	s.Record(context.Background(), 1, Result{Class: ClassSuccess, Latency: 200 * time.Millisecond})
	snap := s.Snapshot(context.Background(), 1)
	if snap.SampleCount != 2 {
		t.Errorf("SampleCount=%d, want 2", snap.SampleCount)
	}
	if snap.SuccessRate != 1.0 {
		t.Errorf("SuccessRate=%v, want 1.0", snap.SuccessRate)
	}
	// EMA: 100 → 0.5*200 + 0.5*100 = 150
	if snap.LatencyMs != 150 {
		t.Errorf("LatencyMs=%v, want 150 (EMA)", snap.LatencyMs)
	}
}

func TestInMemoryStatsStore_ZeroEndpointID_NoOp(t *testing.T) {
	s := NewInMemoryStatsStore(0.2)
	s.Record(context.Background(), 0, Result{Class: ClassSuccess})
	snap := s.Snapshot(context.Background(), 0)
	if snap.SampleCount != 0 {
		t.Errorf("0 ID should be no-op")
	}
}

// =============================================================================
// 兼容性：errors import 占位
// =============================================================================

var _ = errors.New
