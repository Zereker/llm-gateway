package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/internal/dispatch"
	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
	"github.com/zereker/llm-gateway/internal/requeststate"
)

// =============================================================================
// M7 thin-adapter tests
//
// The driver loop's behavior (retry / fallback / verdict / streaming) is tested by
// internal/dispatch; here we only verify the middleware.Schedule wrapper layer:
//   - Dispatcher nil -> panic
//   - M3/M5 not run -> 500
//   - X-Gateway-Max-Attempts header -> passed into dispatch.Input
//   - Outcome -> HTTP code translation
// =============================================================================

// stubSelectorReturns always returns a fixed endpoint or nil.
type stubSelectorReturns struct {
	ep  *domain.Endpoint
	err error
}

func (s stubSelectorReturns) Report(_ context.Context, _ *domain.Endpoint, _ dispatch.Verdict) {}
func (s stubSelectorReturns) Release(_ context.Context, _ *domain.Endpoint)                    {}

func (s stubSelectorReturns) Pick(_ context.Context, _ []*domain.Endpoint, _ dispatch.PickQuery) (*domain.Endpoint, error) {
	return s.ep, s.err
}

// stubCandidates always returns a dummy candidate, letting dispatcher.step get past
// the CandidateSource stage.
type stubCandidates struct{ ep *domain.Endpoint }

func (s stubCandidates) ListForModel(_ context.Context, _, _ string) ([]*domain.Endpoint, error) {
	if s.ep == nil {
		return nil, nil
	}
	return []*domain.Endpoint{s.ep}, nil
}

type stubInvokerFactory struct{ res dispatch.Result }

func (s stubInvokerFactory) For(_ *domain.Endpoint, _ protocol.Handler, _ *domain.RequestEnvelope) dispatch.Invoker {
	return stubInvoker(s)
}

type stubInvoker struct{ res dispatch.Result }

func (s stubInvoker) Invoke(_ context.Context) (dispatch.Result, error) { return s.res, nil }

type stubResult struct {
	verdict dispatch.Verdict
	ep      *domain.Endpoint
}

func (r stubResult) Verdict() dispatch.Verdict  { return r.verdict }
func (r stubResult) Endpoint() *domain.Endpoint { return r.ep }
func (r stubResult) StreamTo(_ context.Context, w http.ResponseWriter) dispatch.StreamReport {
	w.WriteHeader(200)
	_, _ = w.Write([]byte(`{"ok":true}`))
	return dispatch.StreamReport{Usage: &domain.Usage{Total: 1}}
}
func (r stubResult) Close() error { return nil }

func newTestDispatcher(ep *domain.Endpoint, v dispatch.Verdict) *dispatch.Dispatcher {
	return dispatch.New(
		dispatch.WithCandidates(stubCandidates{ep: ep}),
		dispatch.WithSelector(stubSelectorReturns{ep: ep}),
		dispatch.WithInvokerFactory(stubInvokerFactory{res: stubResult{verdict: v, ep: ep}}),
		dispatch.WithCap(dispatch.HeaderAttemptCap{Default: 3}),
		dispatch.WithRetry(dispatch.DefaultRetry{}),
		dispatch.WithFallback(dispatch.ModelChainFallback{}),
	)
}

func attachM7Inputs(model string) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Identity = domain.UserIdentity{AccountID: "a", Group: "default"}
		rc.Envelope = &domain.RequestEnvelope{
			SourceProtocol: domain.ProtoOpenAI,
			Modality:       domain.ModalityChat,
			Model:          model,
			RawBytes:       []byte(`{"model":"` + model + `"}`),
		}
		ms := &domain.ModelService{ID: 1, Model: model}
		rc.ModelService = ms
		rc.ModelChain = []*domain.ModelService{ms}
		// give the dispatcher an always-OK Handler lookup port (the concrete Handler
		// is also just a stub placeholder -- stubInvokerFactory receives it but never
		// calls it). This works around protocol.DefaultLookup having no real
		// adapter / translator registered in the test environment.
		rc.Handlers = stubHandlerLookup{h: stubHandler{}}
		c.Next()
	}
}

// stubHandlerLookup is a test placeholder: returns a fixed stubHandler.
type stubHandlerLookup struct{ h protocol.Handler }

func (s stubHandlerLookup) Get(_ *domain.Endpoint, _ domain.Protocol) protocol.Handler { return s.h }

// stubHandler is a placeholder Handler; in this test suite stubInvokerFactory never
// actually calls its methods.
type stubHandler struct{}

func (stubHandler) Capabilities() protocol.Capabilities { return protocol.Capabilities{} }
func (stubHandler) PrepareCall(_ context.Context, _ *domain.Endpoint, _ []byte) (*protocol.Call, error) {
	return nil, nil
}
func (stubHandler) NewResponseStream() protocol.ResponseStream { return nil }

func runSchedule(t *testing.T, mw gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.POST("/x",
		TraceContext(),
		Recover(),
		attachRCM7(),
		attachM7Inputs("gpt-4"),
		mw,
	)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("POST", "/x", strings.NewReader(`{}`)))
	return w
}

// attachRCM7 attaches a RequestContext, simulating post-M1 state.
func attachRCM7() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("rc", &requeststate.State{})
		c.Next()
	}
}

// sliceEq is used by model_service_test.go -- it used to live in a since-deleted
// schedule_test.go.
func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if v != b[i] {
			return false
		}
	}
	return true
}

// =============================================================================
// Tests
// =============================================================================

func TestSchedule_PanicsOnNilDispatcher(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil Dispatcher")
		}
	}()
	Schedule(nil)
}

func TestSchedule_SuccessStreams200(t *testing.T) {
	ep := &domain.Endpoint{ID: 1, Vendor: "openai", Model: "gpt-4"}
	d := newTestDispatcher(ep, dispatch.Verdict{Class: dispatch.ClassSuccess, HTTPCode: 200})
	w := runSchedule(t, Schedule(d))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestSchedule_InvalidReturns400(t *testing.T) {
	ep := &domain.Endpoint{ID: 1, Vendor: "openai", Model: "gpt-4"}
	d := newTestDispatcher(ep, dispatch.Verdict{Class: dispatch.ClassInvalid, HTTPCode: 400, Reason: "bad body"})
	w := runSchedule(t, Schedule(d))
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestSchedule_MissingRCFields500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	ep := &domain.Endpoint{ID: 1}
	d := newTestDispatcher(ep, dispatch.Verdict{Class: dispatch.ClassSuccess})
	// **not** wiring attachM7Inputs here -- rc.Envelope / ModelChain are left unset
	e.POST("/x", TraceContext(), Recover(), attachRCM7(), Schedule(d))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("POST", "/x", strings.NewReader(`{}`)))
	if w.Code != 500 {
		t.Fatalf("status = %d, want 500 (M3/M5 missing)", w.Code)
	}
}
