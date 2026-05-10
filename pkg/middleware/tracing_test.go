package middleware

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/usage"
)

// stubOutbox captures published events.
type stubOutbox struct {
	mu     sync.Mutex
	events []*usage.OutboxEvent
}

func (o *stubOutbox) Publish(_ context.Context, evt *usage.OutboxEvent) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, evt)
	return nil
}

func TestTracing_PublishesUsageWhenSet(t *testing.T) {
	out := &stubOutbox{}

	r := newGinTest(
		TraceContext(),
		Tracing(TracingDeps{Outbox: out}),
	)
	r.GET("/x", func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Endpoint = &domain.Endpoint{ID: 42, Name: "ep1"}
		rc.Usage = &domain.Usage{Input: 100, Output: 50, Total: 150}
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))

	out.mu.Lock()
	defer out.mu.Unlock()
	if len(out.events) != 1 {
		t.Fatalf("got %d events, want 1", len(out.events))
	}
	if out.events[0].Key != "42" {
		t.Errorf("event key = %q, want 42", out.events[0].Key)
	}
	body := string(out.events[0].Payload)
	if body == "" || !contains(body, `"Total":150`) {
		t.Errorf("payload missing Total: %s", body)
	}
}

func TestTracing_NoUsageNoPublish(t *testing.T) {
	out := &stubOutbox{}
	r := newGinTest(
		TraceContext(),
		Tracing(TracingDeps{Outbox: out}),
	)
	r.GET("/x", func(c *gin.Context) {
		// no Usage set
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))

	out.mu.Lock()
	defer out.mu.Unlock()
	if len(out.events) != 0 {
		t.Errorf("got %d events, want 0", len(out.events))
	}
}

func TestTracing_NoRCHandledGracefully(t *testing.T) {
	out := &stubOutbox{}
	// Note: no TraceContext registered; Tracing should not panic.
	r := newGinTest(Tracing(TracingDeps{Outbox: out}))
	r.GET("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestTracing_OutboxNilTolerated(t *testing.T) {
	// nil outbox should just skip publish without panic
	r := newGinTest(
		TraceContext(),
		Tracing(TracingDeps{Outbox: nil}),
	)
	r.GET("/x", func(c *gin.Context) {
		GetRequestContext(c).Usage = &domain.Usage{Total: 10}
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))

	if w.Code != 200 {
		t.Errorf("status = %d", w.Code)
	}
}

// helper, avoids importing strings just for one Contains call
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
