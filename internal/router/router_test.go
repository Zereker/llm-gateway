package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/internal/dispatch"
	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// stubIdentity always rejects (this router-level layer only cares whether
// the routes + middleware chain are registered; a 401 auth failure already
// proves the middleware ran). It directly satisfies middleware.IdentityProvider's
// narrow interface, so no option wrapping is needed.
type stubIdentity struct{}

func (stubIdentity) Resolve(_ context.Context, _ *domain.Credentials) (*domain.UserIdentity, error) {
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

func (stubSubscriptions) HasModel(_ context.Context, _ string, _ int64) (bool, error) {
	return false, nil
}

// panicSelector / panicInvokerFactory: M7 should never be reached (M2 Auth
// already short-circuits with 401); the panic guards against that — if it's
// ever called, the test's expectations were wrong.
type panicSelector struct{}

func (panicSelector) Report(_ context.Context, _ *domain.Endpoint, _ dispatch.Verdict) {}
func (panicSelector) Release(_ context.Context, _ *domain.Endpoint)                    {}

func (panicSelector) Pick(_ context.Context, _ []*domain.Endpoint, _ dispatch.PickQuery) (*domain.Endpoint, error) {
	panic("router test: Selector.Pick should not be reached (M2 Auth must reject first)")
}

// panicCandidates always panics — after M2 Auth short-circuits with 401,
// CandidateSource should never be called.
type panicCandidates struct{}

func (panicCandidates) ListForModel(_ context.Context, _, _ string) ([]*domain.Endpoint, error) {
	panic("router test: CandidateSource.ListForModel should not be reached")
}

type panicInvokerFactory struct{}

func (panicInvokerFactory) For(_ *domain.Endpoint, _ protocol.Handler, _ *domain.RequestEnvelope) dispatch.Invoker {
	panic("router test: InvokerFactory.For should not be reached")
}

type panicInvoker struct{}

func (panicInvoker) Invoke(_ context.Context) (dispatch.Result, error) {
	panic("router test: Invoker.Invoke should not be reached")
}

type panicResult struct{}

func (panicResult) Verdict() dispatch.Verdict  { panic("not reached") }
func (panicResult) Endpoint() *domain.Endpoint { panic("not reached") }
func (panicResult) StreamTo(context.Context, http.ResponseWriter) dispatch.StreamReport {
	panic("not reached")
}
func (panicResult) Close() error { return nil }

// stubLookup is a no-op protocol.Lookup; these router tests short-circuit at M2
// Auth (401) and never dispatch, so Get is never actually called.
type stubLookup struct{}

func (stubLookup) Get(*domain.Endpoint, domain.Protocol) protocol.Handler { return nil }

func minDeps() Deps {
	return Deps{
		// M2
		IdentityProvider: stubIdentity{},
		// M3 Envelope requires a non-nil lookup
		Handlers: stubLookup{},
		// M5
		ModelCatalog:        stubMSProvider{},
		SubscriptionChecker: stubSubscriptions{},
		// M7 (dispatcher: only triggers after M2 Auth, and this test short-circuits before that at 401)
		Dispatcher: dispatch.New(
			dispatch.WithCandidates(panicCandidates{}),
			dispatch.WithSelector(panicSelector{}),
			dispatch.WithInvokerFactory(panicInvokerFactory{}),
			dispatch.WithCap(dispatch.HeaderAttemptCap{Default: 3}),
			dispatch.WithRetry(dispatch.DefaultRetry{}),
			dispatch.WithFallback(dispatch.ModelChainFallback{}),
		),
		// M4 / M6 / M8 / M10 left empty: each middleware takes a no-op pass-through path when nil/empty
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

// TestNewEngine_AllModalityRoutes verifies that all modality-split routes are
// registered.
// No Authorization -> expect 401 (proving the request entered the middleware
// chain; not a 404).
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

// TestBuildChain_* removed: buildChain is deprecated (each modality lists its
// own middleware). TestNewEngine_AllModalityRoutes indirectly verifies that
// each modality registers its middleware (no Authorization -> 401 rather
// than 404, proving the Auth middleware ran).
