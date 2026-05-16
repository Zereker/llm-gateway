package middleware

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/repo"
	"github.com/zereker/llm-gateway/pkg/schedule"
	"github.com/zereker/llm-gateway/pkg/upstream"
)

// =============================================================================
// stub: EndpointReader
// =============================================================================

type stubEndpointReader struct {
	eps []*domain.Endpoint
	err error
}

func (s stubEndpointReader) ListForModel(_ context.Context, _, _ string) ([]*domain.Endpoint, error) {
	return s.eps, s.err
}
func (s stubEndpointReader) PickForModel(_ context.Context, _, _ string) (*domain.Endpoint, error) {
	return nil, nil
}
func (s stubEndpointReader) GetByID(_ context.Context, _ int64) (*domain.Endpoint, error) {
	return nil, nil
}
func (s stubEndpointReader) List(_ context.Context) ([]*domain.Endpoint, error) { return nil, nil }

// =============================================================================
// stub: schedule.Scheduler / Selection
// =============================================================================

// stubSelection 简单 Selection：按 series 顺序返回 ep；Report 都记录。
type stubSelection struct {
	series     []*domain.Endpoint
	idx        int
	decisions  []schedule.Decision
	beginErr   error
	reportLast schedule.Result
}

func (s *stubSelection) Pick() *domain.Endpoint {
	if s.idx >= len(s.series) {
		return nil
	}
	ep := s.series[s.idx]
	s.idx++
	return ep
}
func (s *stubSelection) Report(ep *domain.Endpoint, r schedule.Result) {
	s.reportLast = r
	if ep != nil {
		s.decisions = append(s.decisions, schedule.Decision{
			AttemptNum: len(s.decisions) + 1,
			EndpointID: ep.ID,
			Result:     r,
		})
	}
}
func (s *stubSelection) Decisions() []schedule.Decision { return s.decisions }
func (s *stubSelection) Done()                          {}

type stubScheduler struct {
	sel      schedule.Selection
	beginErr error
}

func (s stubScheduler) BeginSelection(_ context.Context, _ *schedule.Request) (schedule.Selection, error) {
	return s.sel, s.beginErr
}

// =============================================================================
// attachM7Inputs：M5 / M6 之后的状态（Identity + Envelope + ModelService + RateLimit）
// =============================================================================

func attachM7Inputs(model string) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Identity = domain.UserIdentity{AccountID: "a", SubAccountID: "s", Group: "default"}
		rc.Envelope = &domain.RequestEnvelope{
			SourceProtocol: domain.ProtoOpenAI,
			Modality:       domain.ModalityChat,
			Model:          model,
			RawBytes:       []byte(`{"model":"` + model + `"}`),
		}
		rc.ModelService = &repo.ModelService{ID: 1, Model: model}
		rc.RateLimit = &domain.RateLimitState{}
		c.Next()
	}
}

// =============================================================================
// 失败 / 装配契约 tests
// =============================================================================

func TestSchedule_500_M3orM5Missing(t *testing.T) {
	deps := ScheduleDeps{
		Endpoints: stubEndpointReader{},
		Scheduler: stubScheduler{},
		Sender:    upstream.New(),
	}
	r := newGinTest(TraceContext(), Recover(), Schedule(deps))
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 500 {
		t.Fatalf("status=%d, want=500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "M3/M5 did not run") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestSchedule_ListError_503(t *testing.T) {
	deps := ScheduleDeps{
		Endpoints: stubEndpointReader{err: errors.New("db down")},
		Scheduler: stubScheduler{},
		Sender:    upstream.New(),
	}
	r := newGinTest(TraceContext(), Recover(),
		attachM7Inputs("gpt-4o"),
		Schedule(deps),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 503 {
		t.Fatalf("status=%d, want=503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "list endpoints") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestSchedule_NoCandidates_503(t *testing.T) {
	deps := ScheduleDeps{
		Endpoints: stubEndpointReader{eps: nil},
		Scheduler: stubScheduler{},
		Sender:    upstream.New(),
	}
	r := newGinTest(TraceContext(), Recover(),
		attachM7Inputs("gpt-4o"),
		Schedule(deps),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 503 {
		t.Fatalf("status=%d, want=503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no endpoint for model") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestSchedule_BeginSelectionError_503(t *testing.T) {
	deps := ScheduleDeps{
		Endpoints: stubEndpointReader{eps: []*domain.Endpoint{{ID: 1, Vendor: "openai", Weight: 100}}},
		Scheduler: stubScheduler{beginErr: errors.New("schedule init failed")},
		Sender:    upstream.New(),
	}
	r := newGinTest(TraceContext(), Recover(),
		attachM7Inputs("gpt-4o"),
		Schedule(deps),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 503 {
		t.Fatalf("status=%d, want=503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "schedule init failed") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestSchedule_NoEndpointSucceeded_503(t *testing.T) {
	// stubScheduler 返一个 stubSelection.Pick 永远返 nil → 触发 "no endpoint succeeded"
	deps := ScheduleDeps{
		Endpoints: stubEndpointReader{eps: []*domain.Endpoint{{ID: 1, Vendor: "openai", Weight: 100}}},
		Scheduler: stubScheduler{sel: &stubSelection{}},
		Sender:    upstream.New(),
	}
	r := newGinTest(TraceContext(), Recover(),
		attachM7Inputs("gpt-4o"),
		Schedule(deps),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 503 {
		t.Fatalf("status=%d, want=503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no endpoint succeeded") {
		t.Errorf("body=%s", w.Body.String())
	}
}

// =============================================================================
// header 解析（直接调内部函数）
// =============================================================================

func TestParseMaxAttempts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		hdr  string
		want int
	}{
		{"", 0},
		{"abc", 0},
		{"-3", 0},
		{"0", 0},
		{"5", 5},
		{"100", 100},
	}
	for _, tc := range cases {
		r := gin.New()
		var got int
		r.GET("/x", func(c *gin.Context) {
			got = parseMaxAttempts(c)
			c.Status(200)
		})
		req := httptest.NewRequest("GET", "/x", nil)
		if tc.hdr != "" {
			req.Header.Set(HeaderGatewayMaxAttempts, tc.hdr)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if got != tc.want {
			t.Errorf("hdr=%q: got=%d, want=%d", tc.hdr, got, tc.want)
		}
	}
}

func TestParseFallbackModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		hdr  string
		want []string
	}{
		{"", nil},
		{",,,", nil},
		{"gpt-4o", []string{"gpt-4o"}},
		{"gpt-4o,gpt-4-turbo", []string{"gpt-4o", "gpt-4-turbo"}},
		{"  gpt-4o ,  claude-3 , ", []string{"gpt-4o", "claude-3"}},
	}
	for _, tc := range cases {
		r := gin.New()
		var got []string
		r.GET("/x", func(c *gin.Context) {
			got = parseFallbackModels(c)
			c.Status(200)
		})
		req := httptest.NewRequest("GET", "/x", nil)
		if tc.hdr != "" {
			req.Header.Set(HeaderGatewayFallbackModels, tc.hdr)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if !sliceEq(got, tc.want) {
			t.Errorf("hdr=%q: got=%+v, want=%+v", tc.hdr, got, tc.want)
		}
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPrefixKeyFromBody(t *testing.T) {
	// 空 body → nil
	if got := prefixKeyFromBody(nil); got != nil {
		t.Errorf("empty → got=%v", got)
	}
	// 短 body → 原样
	short := []byte("hello")
	if got := prefixKeyFromBody(short); string(got) != "hello" {
		t.Errorf("short body modified: %q", got)
	}
	// 长 body → 截断到 4KiB
	long := make([]byte, 5*1024)
	for i := range long {
		long[i] = 'a'
	}
	got := prefixKeyFromBody(long)
	if len(got) != 4*1024 {
		t.Errorf("len=%d, want=4096", len(got))
	}
}

func TestConvertDecisions(t *testing.T) {
	decs := []schedule.Decision{
		{AttemptNum: 1, EndpointID: 10, Result: schedule.Result{Class: schedule.ClassTransient}},
		{AttemptNum: 2, EndpointID: 20, Result: schedule.Result{Class: schedule.ClassSuccess}},
	}
	got := convertDecisions(decs)
	if len(got) != 2 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].Outcome != domain.AttemptFallback {
		t.Errorf("decs[0].Outcome=%v, want=fallback (failed but not last)", got[0].Outcome)
	}
	if got[1].Outcome != domain.AttemptSuccess {
		t.Errorf("decs[1].Outcome=%v, want=success", got[1].Outcome)
	}
}

func TestConvertDecisions_AllFailLastIsFail(t *testing.T) {
	decs := []schedule.Decision{
		{AttemptNum: 1, EndpointID: 1, Result: schedule.Result{Class: schedule.ClassTransient}},
		{AttemptNum: 2, EndpointID: 2, Result: schedule.Result{Class: schedule.ClassPermanent}},
	}
	got := convertDecisions(decs)
	if got[len(got)-1].Outcome != domain.AttemptFail {
		t.Errorf("last decision outcome=%v, want=fail", got[len(got)-1].Outcome)
	}
}
