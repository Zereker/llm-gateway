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
// stub: Filter
// =============================================================================

// stubFilter 简单 filter：把 candidates 按 ID 过滤掉 excludeIDs 中的 ep。
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

// pickFirstSelector 永远返回输入候选的第一个；用来代替 WeightedRandom 做确定性测试
type pickFirstSelector struct{}

func (pickFirstSelector) Name() string { return "pickfirst" }
func (pickFirstSelector) Apply(_ context.Context, cands []*domain.Endpoint, _ *Request) []*domain.Endpoint {
	if len(cands) == 0 {
		return nil
	}
	return []*domain.Endpoint{cands[0]}
}

// =============================================================================
// stub: CooldownManager
// =============================================================================

type stubCooldown struct {
	marks       []markCall
	cooled      map[int64]bool
	inCooldown  func(ids []int64) (map[int64]bool, error)
	markErr     error
	mu          sync.Mutex
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

// =============================================================================
// helper: ep ctor
// =============================================================================

func ep(id int64, weight uint32) *domain.Endpoint {
	return &repo.Endpoint{ID: id, Vendor: "openai", Model: "gpt-4o", Weight: weight}
}

// =============================================================================
// ErrorClass tests
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
// BeginSelection 入参校验
// =============================================================================

func TestBeginSelection_NoModel_Error(t *testing.T) {
	s := New(Config{Filters: []Filter{pickFirstSelector{}}})
	_, err := s.BeginSelection(context.Background(), &Request{Model: ""})
	if err == nil {
		t.Fatal("expected err for empty model")
	}
}

func TestBeginSelection_NoCandidates_Error(t *testing.T) {
	s := New(Config{Filters: []Filter{pickFirstSelector{}}})
	_, err := s.BeginSelection(context.Background(), &Request{Model: "x"})
	if err == nil {
		t.Fatal("expected err for no candidates")
	}
}

func TestBeginSelection_MaxAttemptsHeaderOverride_TighterOnly(t *testing.T) {
	s := New(Config{Filters: []Filter{pickFirstSelector{}}, MaxAttempts: 5})

	// override 3 (smaller than 5) → 用 3
	selRaw, _ := s.BeginSelection(context.Background(), &Request{
		Model:               "x",
		Candidates:          []*domain.Endpoint{ep(1, 100)},
		MaxAttemptsOverride: 3,
	})
	sel := selRaw.(*defaultSelection)
	if sel.maxAttempts != 3 {
		t.Errorf("maxAttempts=%d, want=3", sel.maxAttempts)
	}

	// override 10 (larger than 5) → 忽略，保留 5
	selRaw2, _ := s.BeginSelection(context.Background(), &Request{
		Model:               "x",
		Candidates:          []*domain.Endpoint{ep(1, 100)},
		MaxAttemptsOverride: 10,
	})
	sel2 := selRaw2.(*defaultSelection)
	if sel2.maxAttempts != 5 {
		t.Errorf("maxAttempts=%d, want=5 (header larger ignored)", sel2.maxAttempts)
	}
}

// =============================================================================
// Pick: 单 ep happy path
// =============================================================================

func TestPick_SingleEndpoint_Success(t *testing.T) {
	s := New(Config{Filters: []Filter{pickFirstSelector{}}, MaxAttempts: 3})
	sel, _ := s.BeginSelection(context.Background(), &Request{
		Model:      "x",
		Candidates: []*domain.Endpoint{ep(1, 100)},
	})
	defer sel.Done()

	got := sel.Pick()
	if got == nil || got.ID != 1 {
		t.Errorf("Pick=%v, want ep id=1", got)
	}
	sel.Report(got, Result{Class: ClassSuccess})

	decisions := sel.Decisions()
	if len(decisions) != 1 || decisions[0].Result.Class != ClassSuccess {
		t.Errorf("decisions=%+v", decisions)
	}
}

func TestPick_MaxAttemptsExhausted_ReturnsNil(t *testing.T) {
	// 两个 ep，max_attempts=1 → 第二次 Pick 返 nil
	s := New(Config{Filters: []Filter{pickFirstSelector{}}, MaxAttempts: 1})
	sel, _ := s.BeginSelection(context.Background(), &Request{
		Model:      "x",
		Candidates: []*domain.Endpoint{ep(1, 100), ep(2, 100)},
	})
	defer sel.Done()

	first := sel.Pick()
	sel.Report(first, Result{Class: ClassTransient})

	second := sel.Pick()
	if second != nil {
		t.Errorf("second Pick should be nil after max_attempts=1, got %v", second)
	}
}

// =============================================================================
// L1 retry：同 ep transient + MaxPerEndpoint
// =============================================================================

func TestPick_L1Retry_SameEndpointOnTransient(t *testing.T) {
	cd := &stubCooldown{}
	s := New(Config{
		Filters:        []Filter{pickFirstSelector{}},
		Cooldown:       cd,
		MaxAttempts:    5,
		MaxPerEndpoint: 3,
	})
	sel, _ := s.BeginSelection(context.Background(), &Request{
		Model:      "x",
		Candidates: []*domain.Endpoint{ep(1, 100), ep(2, 100)},
	})
	defer sel.Done()

	first := sel.Pick()
	sel.Report(first, Result{Class: ClassTransient})

	second := sel.Pick()
	if second == nil || second.ID != first.ID {
		t.Errorf("expected L1 same-ep retry, got %v vs first=%v", second, first)
	}
	// 不应触发 cooldown.Mark（L1 retry 路径）
	if len(cd.marks) != 0 {
		t.Errorf("L1 retry should not Mark cooldown; got %+v", cd.marks)
	}
}

func TestReport_NonTransient_TriggersCooldown(t *testing.T) {
	cd := &stubCooldown{}
	s := New(Config{
		Filters:        []Filter{pickFirstSelector{}},
		Cooldown:       cd,
		MaxAttempts:    5,
		MaxPerEndpoint: 3,
	})
	sel, _ := s.BeginSelection(context.Background(), &Request{
		Model:      "x",
		Candidates: []*domain.Endpoint{ep(1, 100), ep(2, 100)},
	})
	defer sel.Done()

	first := sel.Pick()
	sel.Report(first, Result{Class: ClassCapacity})

	if len(cd.marks) != 1 || cd.marks[0].ID != first.ID || cd.marks[0].Class != ClassCapacity {
		t.Errorf("expected Mark on capacity failure, got %+v", cd.marks)
	}
}

func TestPick_L1Exhausted_MovesToNextEp(t *testing.T) {
	// MaxPerEndpoint=2，3 次 transient → 第三次 Pick 应换 ep2
	cd := &stubCooldown{}
	s := New(Config{
		Filters:        []Filter{pickFirstSelector{}},
		Cooldown:       cd,
		MaxAttempts:    5,
		MaxPerEndpoint: 2,
	})
	sel, _ := s.BeginSelection(context.Background(), &Request{
		Model:      "x",
		Candidates: []*domain.Endpoint{ep(1, 100), ep(2, 100)},
	})
	defer sel.Done()

	first := sel.Pick()  // ep1 attempt 1
	sel.Report(first, Result{Class: ClassTransient})

	second := sel.Pick() // ep1 attempt 2 (L1 retry)
	sel.Report(second, Result{Class: ClassTransient})

	third := sel.Pick() // 应该换 ep2
	if third == nil || third.ID == first.ID {
		t.Errorf("expected to switch ep, got id=%v (first id=%v)", third, first.ID)
	}
}

// =============================================================================
// Report: Success / Invalid 不冷却
// =============================================================================

func TestReport_Success_NoCooldown(t *testing.T) {
	cd := &stubCooldown{}
	s := New(Config{Filters: []Filter{pickFirstSelector{}}, Cooldown: cd})
	sel, _ := s.BeginSelection(context.Background(), &Request{
		Model:      "x",
		Candidates: []*domain.Endpoint{ep(1, 100)},
	})

	got := sel.Pick()
	sel.Report(got, Result{Class: ClassSuccess})

	if len(cd.marks) != 0 {
		t.Errorf("Success must not Mark, got %+v", cd.marks)
	}
}

func TestReport_Invalid_NoCooldown(t *testing.T) {
	cd := &stubCooldown{}
	s := New(Config{Filters: []Filter{pickFirstSelector{}}, Cooldown: cd})
	sel, _ := s.BeginSelection(context.Background(), &Request{
		Model:      "x",
		Candidates: []*domain.Endpoint{ep(1, 100)},
	})

	got := sel.Pick()
	sel.Report(got, Result{Class: ClassInvalid})

	if len(cd.marks) != 0 {
		t.Errorf("Invalid must not Mark, got %+v", cd.marks)
	}
}

func TestReport_NilEp_NoOp(t *testing.T) {
	cd := &stubCooldown{}
	s := New(Config{Filters: []Filter{pickFirstSelector{}}, Cooldown: cd})
	sel, _ := s.BeginSelection(context.Background(), &Request{
		Model:      "x",
		Candidates: []*domain.Endpoint{ep(1, 100)},
	})
	sel.Report(nil, Result{Class: ClassTransient}) // should not panic

	if len(cd.marks) != 0 || len(sel.Decisions()) != 0 {
		t.Errorf("nil ep Report should be no-op")
	}
}

// =============================================================================
// L3 fallback model
// =============================================================================

func TestL3_FallbackModel_LoadsNewCandidates(t *testing.T) {
	cd := &stubCooldown{}
	// chain：filter + selector；当 main model ep1 失败后切到 fallback model 拿 ep10
	s := New(Config{
		Filters:        []Filter{pickFirstSelector{}},
		Cooldown:       cd,
		MaxAttempts:    5,
		MaxPerEndpoint: 1,
	})

	fallbackCalled := 0
	sel, _ := s.BeginSelection(context.Background(), &Request{
		Model:          "primary",
		Candidates:     []*domain.Endpoint{ep(1, 100)},
		FallbackModels: []string{"fallback1"},
		LoadFallback: func(_ context.Context, model, _ string) ([]*domain.Endpoint, error) {
			fallbackCalled++
			if model == "fallback1" {
				return []*domain.Endpoint{ep(10, 100)}, nil
			}
			return nil, nil
		},
	})

	first := sel.Pick()
	if first.ID != 1 {
		t.Fatalf("first pick id=%d", first.ID)
	}
	sel.Report(first, Result{Class: ClassCapacity}) // 非 transient → cooldown，主 model 候选耗尽

	second := sel.Pick()
	if second == nil || second.ID != 10 {
		t.Errorf("expected fallback ep10, got %v", second)
	}
	if fallbackCalled != 1 {
		t.Errorf("LoadFallback calls=%d, want=1", fallbackCalled)
	}
}

func TestL3_NoLoadFallback_ReturnsNil(t *testing.T) {
	s := New(Config{Filters: []Filter{pickFirstSelector{}}, MaxAttempts: 5, MaxPerEndpoint: 1, Cooldown: &stubCooldown{}})
	sel, _ := s.BeginSelection(context.Background(), &Request{
		Model:          "primary",
		Candidates:     []*domain.Endpoint{ep(1, 100)},
		FallbackModels: []string{"fallback1"},
		LoadFallback:   nil, // L3 关闭
	})

	first := sel.Pick()
	sel.Report(first, Result{Class: ClassPermanent})

	if got := sel.Pick(); got != nil {
		t.Errorf("expected nil (LoadFallback nil), got %v", got)
	}
}

func TestL3_SkipsEmptyAndSameModel(t *testing.T) {
	cd := &stubCooldown{}
	s := New(Config{Filters: []Filter{pickFirstSelector{}}, MaxAttempts: 5, MaxPerEndpoint: 1, Cooldown: cd})

	loaded := []string{}
	sel, _ := s.BeginSelection(context.Background(), &Request{
		Model:          "primary",
		Candidates:     []*domain.Endpoint{ep(1, 100)},
		FallbackModels: []string{"", "primary", "real-fallback"},
		LoadFallback: func(_ context.Context, model, _ string) ([]*domain.Endpoint, error) {
			loaded = append(loaded, model)
			if model == "real-fallback" {
				return []*domain.Endpoint{ep(20, 100)}, nil
			}
			return nil, nil
		},
	})

	first := sel.Pick()
	sel.Report(first, Result{Class: ClassCapacity})

	got := sel.Pick()
	if got == nil || got.ID != 20 {
		t.Errorf("expected ep20, got %v", got)
	}
	if len(loaded) != 1 || loaded[0] != "real-fallback" {
		t.Errorf("LoadFallback should skip empty + same-model, only called for real-fallback; got %+v", loaded)
	}
}

func TestL3_LoadFallback_ErrorOrEmpty_TriesNext(t *testing.T) {
	cd := &stubCooldown{}
	s := New(Config{Filters: []Filter{pickFirstSelector{}}, MaxAttempts: 5, MaxPerEndpoint: 1, Cooldown: cd})

	sel, _ := s.BeginSelection(context.Background(), &Request{
		Model:          "primary",
		Candidates:     []*domain.Endpoint{ep(1, 100)},
		FallbackModels: []string{"err-model", "empty-model", "ok-model"},
		LoadFallback: func(_ context.Context, model, _ string) ([]*domain.Endpoint, error) {
			switch model {
			case "err-model":
				return nil, errors.New("db down")
			case "empty-model":
				return nil, nil
			default:
				return []*domain.Endpoint{ep(99, 100)}, nil
			}
		},
	})

	first := sel.Pick()
	sel.Report(first, Result{Class: ClassPermanent})

	got := sel.Pick()
	if got == nil || got.ID != 99 {
		t.Errorf("expected ok-model ep99, got %v", got)
	}
}

// =============================================================================
// Decisions: 返回 copy + AttemptNum 递增
// =============================================================================

func TestDecisions_AttemptNumsAndCopy(t *testing.T) {
	s := New(Config{Filters: []Filter{pickFirstSelector{}}, MaxAttempts: 5, MaxPerEndpoint: 1, Cooldown: &stubCooldown{}})
	sel, _ := s.BeginSelection(context.Background(), &Request{
		Model:      "x",
		Candidates: []*domain.Endpoint{ep(1, 100), ep(2, 100), ep(3, 100)},
	})
	for i := 0; i < 3; i++ {
		got := sel.Pick()
		if got == nil {
			break
		}
		sel.Report(got, Result{Class: ClassPermanent, Latency: time.Duration(i+1) * time.Millisecond})
	}

	d := sel.Decisions()
	if len(d) != 3 {
		t.Fatalf("decisions=%d, want=3", len(d))
	}
	for i, dec := range d {
		if dec.AttemptNum != i+1 {
			t.Errorf("decisions[%d].AttemptNum=%d, want=%d", i, dec.AttemptNum, i+1)
		}
	}

	// 验证 Decisions 返回的是 copy
	d[0].EndpointID = 9999
	d2 := sel.Decisions()
	if d2[0].EndpointID == 9999 {
		t.Error("Decisions should return a copy, mutation leaked")
	}
}
