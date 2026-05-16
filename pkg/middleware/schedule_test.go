package middleware

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	eps map[string][]*domain.Endpoint // model → endpoints
	err error
}

func (s stubEndpointReader) ListForModel(_ context.Context, model, _ string) ([]*domain.Endpoint, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.eps == nil {
		return nil, nil
	}
	return s.eps[model], nil
}

// =============================================================================
// stub: schedule.Scheduler（无状态 Pick + Report）
// =============================================================================

type stubScheduler struct {
	picks   []*domain.Endpoint // 按顺序返回；用尽则返 nil
	idx     atomic.Int32
	pickErr error
	reports []reportEntry
}

type reportEntry struct {
	EpID int64
	Cls  schedule.ErrorClass
}

func (s *stubScheduler) Pick(_ context.Context, req *schedule.Request) (*domain.Endpoint, error) {
	if s.pickErr != nil {
		return nil, s.pickErr
	}
	for {
		i := int(s.idx.Load())
		if i >= len(s.picks) {
			return nil, nil
		}
		s.idx.Add(1)
		ep := s.picks[i]
		if ep == nil {
			continue
		}
		// 尊重 ExcludeIDs
		if _, excluded := req.ExcludeIDs[ep.ID]; excluded {
			continue
		}
		return ep, nil
	}
}

func (s *stubScheduler) Report(_ context.Context, ep *domain.Endpoint, r schedule.Result) {
	if ep != nil {
		s.reports = append(s.reports, reportEntry{EpID: ep.ID, Cls: r.Class})
	}
}

// =============================================================================
// attachM7Inputs
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

func defaultScheduleOpts(scheduler schedule.Scheduler, eps map[string][]*domain.Endpoint) []ScheduleOption {
	return []ScheduleOption{
		WithEndpointReader(stubEndpointReader{eps: eps}),
		WithFallbackCatalog(stubCatalog{ms: &repo.ModelService{ID: 1, Model: "gpt-4o"}}),
		WithFallbackSubscriptionChecker(stubSubs{has: true}),
		WithScheduler(scheduler),
		WithSender(upstream.New()),
		WithMaxAttempts(3),
	}
}

// =============================================================================
// 装配契约
// =============================================================================

func TestSchedule_500_M3orM5Missing(t *testing.T) {
	r := newGinTest(TraceContext(), Recover(), Schedule(
		WithEndpointReader(stubEndpointReader{}),
		WithFallbackCatalog(stubCatalog{}),
		WithFallbackSubscriptionChecker(stubSubs{has: true}),
		WithScheduler(&stubScheduler{}),
		WithSender(upstream.New()),
	))
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 500 {
		t.Fatalf("status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "M3/M5 did not run") {
		t.Errorf("body=%s", w.Body.String())
	}
}

// =============================================================================
// list / candidates 失败路径
// =============================================================================

func TestSchedule_ListError_503(t *testing.T) {
	r := newGinTest(TraceContext(), Recover(), attachM7Inputs("gpt-4o"), Schedule(
		WithEndpointReader(stubEndpointReader{err: errors.New("db down")}),
		WithFallbackCatalog(stubCatalog{ms: &repo.ModelService{ID: 1}}),
		WithFallbackSubscriptionChecker(stubSubs{has: true}),
		WithScheduler(&stubScheduler{}),
		WithSender(upstream.New()),
		WithMaxAttempts(3),
	))
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 503 {
		t.Fatalf("status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "list endpoints") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestSchedule_NoEndpointAtAll_503(t *testing.T) {
	opts := defaultScheduleOpts(&stubScheduler{}, map[string][]*domain.Endpoint{})
	r := newGinTest(TraceContext(), Recover(), attachM7Inputs("gpt-4o"), Schedule(opts...))
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 503 {
		t.Fatalf("status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no_endpoint_available") {
		t.Errorf("body=%s", w.Body.String())
	}
}

// =============================================================================
// header 解析
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
		// 去重保序（docs/03 §5）
		{"a,b,a,c", []string{"a", "b", "c"}},
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

// =============================================================================
// routedModelOf
// =============================================================================

func TestRoutedModelOf(t *testing.T) {
	rc := &domain.RequestContext{}
	if routedModelOf(rc) != "" {
		t.Error("empty RC should return empty")
	}
	rc.ModelService = &repo.ModelService{Model: "primary"}
	if routedModelOf(rc) != "primary" {
		t.Error("falls back to ModelService")
	}
	rc.RoutedModelService = &repo.ModelService{Model: "routed"}
	if routedModelOf(rc) != "routed" {
		t.Error("prefers RoutedModelService")
	}
}
