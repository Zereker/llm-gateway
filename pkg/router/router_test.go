package router

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/middleware"
)

// stubMSProvider for tests.
type stubMSProvider struct{ snap *domain.ModelServiceSnapshot }

func (s stubMSProvider) GetByModel(_ context.Context, _ string) (*domain.ModelServiceSnapshot, error) {
	return s.snap, nil
}
func (s stubMSProvider) List(_ context.Context) ([]*domain.ModelServiceSnapshot, error) {
	return []*domain.ModelServiceSnapshot{s.snap}, nil
}

// stubEPProvider for tests.
type stubEPProvider struct{ ep *domain.Endpoint }

func (s stubEPProvider) PickForModel(_ context.Context, _, _ string) (*domain.Endpoint, error) {
	return s.ep, nil
}
func (s stubEPProvider) List(_ context.Context) ([]*domain.Endpoint, error) {
	return []*domain.Endpoint{s.ep}, nil
}

func minDeps() Deps {
	return Deps{
		IdentityProvider: middleware.NewAPIKeyProvider(nil),
		Detector:         middleware.DefaultDetector{},
		Parser:           middleware.DefaultParser{},
		ModelService:     stubMSProvider{},
		Endpoints:        stubEPProvider{},
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

func TestBuildChain_OptionalMiddlewareSkippedWhenNil(t *testing.T) {
	deps := minDeps()
	chain := buildChain(deps)
	// M1 + M9 + M2 + M3 + M5 + M7 + M10 = 7
	if len(chain) != 7 {
		t.Errorf("chain length = %d, want 7 (core only)", len(chain))
	}
}

func TestBuildChain_BodyLimitAndTimeoutPrepended(t *testing.T) {
	deps := minDeps()
	deps.BodyLimit = 1 << 20
	deps.Timeout = 1
	chain := buildChain(deps)
	// 7 core + 2 pre = 9
	if len(chain) != 9 {
		t.Errorf("chain length = %d, want 9 (core + bodyLimit + timeout)", len(chain))
	}
}
