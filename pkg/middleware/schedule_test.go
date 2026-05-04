package middleware

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/adapter"
	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/schedule"

	// 注册 identity translator (ProtoOpenAI ↔ ProtoOpenAI)，M7 找翻译器要用
	_ "github.com/zereker-labs/ai-gateway/pkg/translator/identity"
)

// === stubs for adapter.Factory / Session（slim 版） ===

type stubFactory struct {
	upstreamURL string
	closed      *bool
}

type stubSession struct {
	url       string
	closedRef *bool
}

func (f stubFactory) Metadata() adapter.Metadata {
	return adapter.Metadata{Vendor: "test", NativeProtocol: domain.ProtoOpenAI}
}

func (f stubFactory) NewSession(_ context.Context, _ *domain.Endpoint, _ *domain.RequestEnvelope) (adapter.Session, error) {
	return &stubSession{
		url:       f.upstreamURL,
		closedRef: f.closed,
	}, nil
}

func (s *stubSession) BuildRequest(body []byte) (*http.Request, error) {
	return http.NewRequest("POST", s.url, bytes.NewReader(body))
}

func (s *stubSession) Close() error {
	if s.closedRef != nil {
		*s.closedRef = true
	}
	return nil
}

// === stub EndpointProvider ===

type stubEPProvider struct {
	ep  *domain.Endpoint
	err error
}

func (s stubEPProvider) ListForModel(_ context.Context, _, _ string) ([]*domain.Endpoint, error) {
	if s.ep == nil {
		return nil, s.err
	}
	return []*domain.Endpoint{s.ep}, s.err
}

func (s stubEPProvider) PickForModel(_ context.Context, _, _ string) (*domain.Endpoint, error) {
	return s.ep, s.err
}

func (s stubEPProvider) GetByID(_ context.Context, _ int64) (*domain.Endpoint, error) {
	return s.ep, s.err
}

func (s stubEPProvider) List(_ context.Context) ([]*domain.Endpoint, error) {
	if s.ep == nil {
		return nil, nil
	}
	return []*domain.Endpoint{s.ep}, nil
}

// makeScheduler 用 stub endpoint provider 构造一个最简 Scheduler（无 cooldown / 无 limit / 直接 weighted）。
func makeScheduler(ep *domain.Endpoint, err error) schedule.Scheduler {
	return schedule.New(schedule.Config{
		Candidates:  stubEPProvider{ep: ep, err: err},
		Filters:     []schedule.Filter{schedule.NewWeightedRandomSelector()},
		MaxAttempts: 3,
	})
}

// installM5 sets rc.Envelope + rc.ModelService to bypass M3+M5
func installM5(model string) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		// 用非流式 body 让 identity translator 不启用 SSE 解析
		body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`)
		rc.Envelope = &domain.RequestEnvelope{
			Parsed:         domain.CanonicalRequest{Model: model, Stream: false},
			SourceProtocol: domain.ProtoOpenAI,
			Modality:       domain.ModalityChat,
			RawBytes:       body,
		}
		rc.ModelService = &domain.ModelServiceSnapshot{Model: model}
		c.Next()
	}
}

// === tests ===

func TestSchedule_HappyPath(t *testing.T) {
	// Upstream returns a JSON body; identity translator passes it through to client + extracts usage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstream.Close()

	closed := false
	factory := stubFactory{upstreamURL: upstream.URL, closed: &closed}
	ep := &domain.Endpoint{ID: 1, Name: "ep1", Vendor: "test", Model: "gpt-4o", Group: "default", Weight: 100}

	r := newGinTest(
		TraceContext(), Recover(),
		installM5("gpt-4o"),
		Schedule(ScheduleDeps{
			Scheduler:  makeScheduler(ep, nil),
			GetFactory: func(_ string) adapter.Factory { return factory },
		}),
	)
	var capturedRC *domain.RequestContext
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		capturedRC = GetRequestContext(c)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`)))

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"content":"ok"`) {
		t.Errorf("body missing content: %s", w.Body.String())
	}
	if !closed {
		t.Error("Session.Close not called")
	}
	if capturedRC.Endpoint != ep {
		t.Error("rc.Endpoint not set")
	}
	// identity translator 提取 usage 后塞 rc.Usage
	if capturedRC.Usage == nil || capturedRC.Usage.Total != 15 {
		t.Errorf("rc.Usage = %+v, want Total=15", capturedRC.Usage)
	}
}

func TestSchedule_NoEndpoint(t *testing.T) {
	r := newGinTest(
		TraceContext(), Recover(),
		installM5("gpt-4o"),
		Schedule(ScheduleDeps{Scheduler: makeScheduler(nil, errors.New("no endpoint"))}),
	)
	r.POST("/x", func(c *gin.Context) {})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))

	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestSchedule_NoFactory(t *testing.T) {
	ep := &domain.Endpoint{ID: 1, Name: "ep1", Vendor: "unknown-vendor", Model: "gpt-4o", Group: "default", Weight: 100}
	r := newGinTest(
		TraceContext(), Recover(),
		installM5("gpt-4o"),
		Schedule(ScheduleDeps{
			Scheduler:  makeScheduler(ep, nil),
			GetFactory: func(_ string) adapter.Factory { return nil },
		}),
	)
	r.POST("/x", func(c *gin.Context) {})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))

	// 没 adapter → Scheduler.Report 标 Permanent → 试下一个候选 → 没了 → 503
	if w.Code != 503 {
		t.Errorf("status = %d, want 503 (no factory exhausts candidates)", w.Code)
	}
}

func TestSchedule_UpstreamError(t *testing.T) {
	closed := false
	ep := &domain.Endpoint{ID: 1, Name: "ep1", Vendor: "test", Model: "gpt-4o", Group: "default", Weight: 100}
	factory := stubFactory{upstreamURL: "http://127.0.0.1:1", closed: &closed}
	r := newGinTest(
		TraceContext(), Recover(),
		installM5("gpt-4o"),
		Schedule(ScheduleDeps{
			Scheduler:  makeScheduler(ep, nil),
			GetFactory: func(_ string) adapter.Factory { return factory },
		}),
	)
	r.POST("/x", func(c *gin.Context) {})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))

	// 上游不可达 → schedule.Result{Class:Transient}；scheduler triedSet 阻止重选同 ep
	// → 候选耗尽 → abort 503（区别于"上游真返 502"那种 forward 502）
	if w.Code != 503 {
		t.Errorf("status = %d, want 503 (transient retry exhausts to 503)", w.Code)
	}
	if !closed {
		t.Error("Session.Close should be called even on error")
	}
}
