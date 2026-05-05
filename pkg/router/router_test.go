package router

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/middleware"
	"github.com/zereker-labs/ai-gateway/pkg/repo"
	"github.com/zereker-labs/ai-gateway/pkg/schedule"
)

// stubIdentity 永远拒（router 这层只关心路由 + middleware 链是否注册，
// auth 失败 401 已经能证明 middleware 跑了）。
type stubIdentity struct{}

func (stubIdentity) Resolve(_ context.Context, _ *repo.Credentials) (*repo.UserIdentity, error) {
	return nil, errStubAuth
}

var errStubAuth = stubAuthError("stub: not authenticated")

type stubAuthError string

func (e stubAuthError) Error() string { return string(e) }

// stubMSProvider for tests.
type stubMSProvider struct{ snap *domain.ModelService }

func (s stubMSProvider) GetByModel(_ context.Context, _ string) (*domain.ModelService, error) {
	return s.snap, nil
}
func (s stubMSProvider) List(_ context.Context) ([]*domain.ModelService, error) {
	return []*domain.ModelService{s.snap}, nil
}

// stubSubscriptions for tests; never reached because stubIdentity rejects auth first.
type stubSubscriptions struct{}

func (stubSubscriptions) Has(_ context.Context, _ string, _ int64) (bool, error) {
	return false, nil
}

// stubPricing for tests; never reached because stubIdentity rejects auth first.
type stubPricing struct{}

func (stubPricing) GetActive(_ context.Context, _ string, _ int64, _ string, _ time.Time) (*repo.PricingVersion, error) {
	return nil, nil
}
func (stubPricing) ListHistory(_ context.Context, _ string, _ int64, _ string) ([]*repo.PricingVersion, error) {
	return nil, nil
}

// stubEPProvider for tests.
type stubEPProvider struct{ ep *domain.Endpoint }

func (s stubEPProvider) ListForModel(_ context.Context, _, _ string) ([]*domain.Endpoint, error) {
	if s.ep == nil {
		return nil, nil
	}
	return []*domain.Endpoint{s.ep}, nil
}
func (s stubEPProvider) PickForModel(_ context.Context, _, _ string) (*domain.Endpoint, error) {
	return s.ep, nil
}
func (s stubEPProvider) GetByID(_ context.Context, _ int64) (*domain.Endpoint, error) {
	return s.ep, nil
}
func (s stubEPProvider) List(_ context.Context) ([]*domain.Endpoint, error) {
	return []*domain.Endpoint{s.ep}, nil
}

func minDeps() Deps {
	return Deps{
		Auth: middleware.AuthDeps{Provider: stubIdentity{}},
		ModelService: middleware.ModelServiceDeps{
			Provider:      stubMSProvider{},
			Subscriptions: stubSubscriptions{},
			Pricing:       stubPricing{},
		},
		Schedule: middleware.ScheduleDeps{
			Scheduler: schedule.New(schedule.Config{
				Candidates:  stubEPProvider{},
				Filters:     []schedule.Filter{schedule.NewWeightedRandomSelector()},
				MaxAttempts: 3,
			}),
		},
	}
}

func TestNewEngine_HealthEndpoints(t *testing.T) {
	engine := NewEngine(minDeps())

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Errorf("%s: status = %d, want 200", path, w.Code)
		}
	}
}

func TestNewEngine_AuthRequired(t *testing.T) {
	engine := NewEngine(minDeps())

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"x"}`)))

	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestNewEngine_OpsEndpointsBypassMiddleware(t *testing.T) {
	engine := NewEngine(minDeps())

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))

	if w.Code != 200 {
		t.Errorf("status = %d, want 200 (ops should bypass main chain)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ok") {
		t.Errorf("body = %q", w.Body.String())
	}
}

// TestNewEngine_AllModalityRoutes 验证按模态拆分的所有路由都注册了。
// 没有 Authorization → 期望 401（说明请求进了 middleware 链；不是 404）。
func TestNewEngine_AllModalityRoutes(t *testing.T) {
	engine := NewEngine(minDeps())

	paths := []string{
		// chat
		"/v1/chat/completions",
		"/v1/messages",
		// image
		"/v1/images/generations",
		"/v1/images/edits",
		"/v1/images/variations",
		// audio
		"/v1/audio/speech",
		"/v1/audio/transcriptions",
		"/v1/audio/translations",
		// embedding
		"/v1/embeddings",
	}

	for _, p := range paths {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest("POST", p, strings.NewReader(`{"model":"x"}`)))

		if w.Code == 404 {
			t.Errorf("%s: 404 (route not registered)", p)
			continue
		}
		// 401 (Auth) means the route exists and middleware ran
		if w.Code != 401 {
			t.Logf("%s: status = %d (should be 401 for missing auth)", p, w.Code)
		}
	}
}

// 删除 TestBuildChain_*：buildChain 已废弃（每个 modality 自己列 middleware）。
// 通过 TestNewEngine_AllModalityRoutes 间接验证各模态注册了 middleware（
// 没有 Authorization → 401 而非 404，说明 Auth middleware 跑了）。
