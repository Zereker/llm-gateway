package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/domain"
)

// =============================================================================
// M7 thin-adapter 测试
//
// driver loop 行为（retry / fallback / verdict / streaming）由 pkg/dispatch 测；
// 这里只验证 middleware.Schedule 这层 wrapper：
//   - Dispatcher nil → panic
//   - M3/M5 未跑 → 500
//   - X-Gateway-Max-Attempts header → 写入 rc.Extras[dispatch.HeaderKey]
//   - Outcome → HTTP code 翻译
// =============================================================================

// stubSelectorReturns 永远返一个固定 endpoint 或 nil。
type stubSelectorReturns struct {
	ep  *domain.Endpoint
	err error
}

func (s stubSelectorReturns) Select(_ context.Context, _ dispatch.Query) (*domain.Endpoint, error) {
	return s.ep, s.err
}

type stubInvokerFactory struct{ res dispatch.Result }

func (s stubInvokerFactory) For(_ *domain.Endpoint, _ *domain.RequestEnvelope, _ []byte) dispatch.Invoker {
	return stubInvoker{res: s.res}
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
		c.Next()
	}
}

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

// attachRCM7 装一个 RequestContext，模拟 M1 之后状态。
func attachRCM7() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("rc", &domain.RequestContext{})
		c.Next()
	}
}

// sliceEq 给 model_service_test.go 用——之前住在删掉的 schedule_test.go 里。
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
// 测试
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
	// 这里**不**挂 attachM7Inputs——rc.Envelope / ModelChain 都没填
	e.POST("/x", TraceContext(), Recover(), attachRCM7(), Schedule(d))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("POST", "/x", strings.NewReader(`{}`)))
	if w.Code != 500 {
		t.Fatalf("status = %d, want 500 (M3/M5 missing)", w.Code)
	}
}
