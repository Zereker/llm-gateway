package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/adapter"
	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// === stubs for adapter.Factory / Session ===

type stubFactory struct {
	upstreamURL string
	feedErr     error
	finalize    adapter.FinalizeResult
	closed      *bool
}

type stubSession struct {
	url       string
	feedErr   error
	finalize  adapter.FinalizeResult
	closedRef *bool
}

func (f stubFactory) Metadata() adapter.Metadata {
	return adapter.Metadata{Vendor: "test", NativeProtocol: domain.ProtoOpenAI}
}

func (f stubFactory) NewSession(_ context.Context, _ *domain.Endpoint, _ *domain.RequestEnvelope) (adapter.Session, error) {
	return &stubSession{
		url:       f.upstreamURL,
		feedErr:   f.feedErr,
		finalize:  f.finalize,
		closedRef: f.closed,
	}, nil
}

func (s *stubSession) BuildRequest() (*http.Request, error) {
	req, _ := http.NewRequest("POST", s.url, nil)
	return req, nil
}

func (s *stubSession) Feed(chunk []byte) ([]byte, error) {
	if s.feedErr != nil {
		return nil, s.feedErr
	}
	return chunk, nil // pass-through
}

func (s *stubSession) Finalize() adapter.FinalizeResult {
	return s.finalize
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

func (s stubEPProvider) PickForModel(_ context.Context, _, _ string) (*domain.Endpoint, error) {
	return s.ep, s.err
}

func (s stubEPProvider) List(_ context.Context) ([]*domain.Endpoint, error) {
	if s.ep == nil {
		return nil, nil
	}
	return []*domain.Endpoint{s.ep}, nil
}

// installM5 sets rc.Envelope + rc.ModelService to bypass M3+M5
func installM5(model string) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Envelope = &domain.RequestEnvelope{
			Parsed:         domain.CanonicalRequest{Model: model},
			SourceProtocol: domain.ProtoOpenAI,
			Modality:       domain.ModalityChat,
		}
		rc.ModelService = &domain.ModelServiceSnapshot{Model: model}
		c.Next()
	}
}

// === tests ===

func TestSchedule_HappyPath(t *testing.T) {
	// Upstream returns a JSON body; Schedule should pass-through it.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	closed := false
	wantUsage := &domain.Usage{Input: 100, Output: 50, Total: 150}
	factory := stubFactory{
		upstreamURL: upstream.URL,
		finalize:    adapter.FinalizeResult{Usage: wantUsage},
		closed:      &closed,
	}

	ep := &domain.Endpoint{ID: "ep1", Vendor: "test", URL: upstream.URL, Model: "gpt-4o", Group: "default"}

	r := newGinTest(
		TraceContext(), Recover(),
		installM5("gpt-4o"),
		Schedule(ScheduleDeps{
			Endpoints:  stubEPProvider{ep: ep},
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
	if w.Body.String() != `{"ok":true}` {
		t.Errorf("body = %s", w.Body.String())
	}
	if !closed {
		t.Error("Session.Close not called")
	}
	if capturedRC.Endpoint != ep {
		t.Error("rc.Endpoint not set")
	}
	if capturedRC.Usage != wantUsage {
		t.Error("rc.Usage not set from Finalize")
	}
}

func TestSchedule_NoEndpoint(t *testing.T) {
	r := newGinTest(
		TraceContext(), Recover(),
		installM5("gpt-4o"),
		Schedule(ScheduleDeps{
			Endpoints: stubEPProvider{err: errors.New("no endpoint")},
		}),
	)
	r.POST("/x", func(c *gin.Context) {})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))

	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestSchedule_NoFactory(t *testing.T) {
	ep := &domain.Endpoint{ID: "ep1", Vendor: "unknown-vendor", Model: "gpt-4o", Group: "default"}
	r := newGinTest(
		TraceContext(), Recover(),
		installM5("gpt-4o"),
		Schedule(ScheduleDeps{
			Endpoints:  stubEPProvider{ep: ep},
			GetFactory: func(_ string) adapter.Factory { return nil },
		}),
	)
	r.POST("/x", func(c *gin.Context) {})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no adapter registered") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestSchedule_UpstreamError(t *testing.T) {
	closed := false
	ep := &domain.Endpoint{ID: "ep1", Vendor: "test", Model: "gpt-4o", Group: "default"}
	factory := stubFactory{
		upstreamURL: "http://127.0.0.1:1", // intentionally unreachable
		closed:      &closed,
	}
	r := newGinTest(
		TraceContext(), Recover(),
		installM5("gpt-4o"),
		Schedule(ScheduleDeps{
			Endpoints:  stubEPProvider{ep: ep},
			GetFactory: func(_ string) adapter.Factory { return factory },
		}),
	)
	r.POST("/x", func(c *gin.Context) {})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))

	if w.Code != 502 {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if !closed {
		t.Error("Session.Close should be called even on error")
	}
}

func TestSchedule_FeedError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	defer upstream.Close()

	closed := false
	ep := &domain.Endpoint{ID: "ep1", Vendor: "test", Model: "gpt-4o", Group: "default"}
	factory := stubFactory{
		upstreamURL: upstream.URL,
		feedErr:     errors.New("parse fail"),
		closed:      &closed,
	}
	r := newGinTest(
		TraceContext(), Recover(),
		installM5("gpt-4o"),
		Schedule(ScheduleDeps{
			Endpoints:  stubEPProvider{ep: ep},
			GetFactory: func(_ string) adapter.Factory { return factory },
		}),
	)
	var rcCaptured *domain.RequestContext
	r.POST("/x", func(c *gin.Context) {
		rcCaptured = GetRequestContext(c)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))

	// upstream status 200 was already written; M9 cannot override
	if w.Code != 200 {
		t.Errorf("status = %d, want 200 (already streamed)", w.Code)
	}
	if rcCaptured.Error == nil {
		t.Error("rc.Error should be set to Feed error")
	}
	if !closed {
		t.Error("Session.Close not called")
	}
}

// Cancellation test omitted from v0.1: gin uses ctx from c.Request directly,
// and httptest.NewRecorder doesn't drive ctx cancellation reliably across
// goroutines. Real cancellation is exercised in step-7 end-to-end tests.
