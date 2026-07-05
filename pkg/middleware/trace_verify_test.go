package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/trace"
)

// installRealTracerProvider installs a TracerProvider for this test that actually
// creates child spans, and returns a SpanRecorder for assertions. The default otel
// global is noop -- noop tracer.Start just returns the original ctx + a noop span
// and never creates a distinct span_id.
func installRealTracerProvider(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return sr
}

// TestE2E_LogsCarryTraceID is an end-to-end check: CtxHandler wraps the slog
// default -> goes through M1 + any downstream middleware -> the handler calls
// slog.InfoContext(c.Request.Context(), ...) -> the log must contain
// trace_id / span_id / request_id (sourced from baggage).
//
// **Regression guard**: after the refactor removed rc.Ctx, every mw changed to
// c.Request = c.Request.WithContext(ctx). This test ensures c.Request.Context() at
// handler time still carries M1's span info -- if any mw forgets to write it back
// to c.Request, trace_id disappears from the logs.
func TestE2E_LogsCarryTraceID(t *testing.T) {
	installRealTracerProvider(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(trace.NewCtxHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	defer slog.SetDefault(prev)

	var handlerTraceID, handlerSpanID, rcRequestID string

	// chain together a "minimal but spanning multiple mw" pipeline: M1 -> M2(auth) -> M3(envelope) -> handler
	r := newGinTest(
		TraceContext(),
		Recover(),
		Auth(WithIdentityProvider(stubE2EIdentity{})),
		WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityChat),
		Envelope(),
	)
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		// inside the handler: c.Request.Context() must carry span info
		ctx := c.Request.Context()
		sc := oteltrace.SpanContextFromContext(ctx)
		handlerTraceID = sc.TraceID().String()
		handlerSpanID = sc.SpanID().String()
		rcRequestID = GetRequestContext(c).RequestID

		slog.InfoContext(ctx, "handler.executed", "where", "business")
		c.Status(200)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer test-token")
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	// 1. the ctx the handler receives must have a valid SpanContext
	if handlerTraceID == "" || handlerTraceID == "00000000000000000000000000000000" {
		t.Errorf("handler ctx missing trace_id: got=%q", handlerTraceID)
	}
	if handlerSpanID == "" || handlerSpanID == "0000000000000000" {
		t.Errorf("handler ctx missing span_id: got=%q", handlerSpanID)
	}
	if rcRequestID == "" {
		t.Errorf("rc.RequestID is empty")
	}

	// 2. every log record must carry trace_id (auto-injected by CtxHandler)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("log line count < 2, output:\n%s", buf.String())
	}
	for i, line := range lines {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("line %d is not JSON: %s", i, line)
			continue
		}
		// trace_id must exist and equal what the handler saw (same root span)
		traceID, _ := rec["trace_id"].(string)
		if traceID == "" {
			t.Errorf("line %d msg=%v missing trace_id", i, rec["msg"])
			continue
		}
		if traceID != handlerTraceID {
			t.Errorf("line %d trace_id=%s, want=%s (should match the handler's root span)",
				i, traceID, handlerTraceID)
		}
		// span_id must exist
		if sid, _ := rec["span_id"].(string); sid == "" {
			t.Errorf("line %d msg=%v missing span_id", i, rec["msg"])
		}
		// request_id (injected into baggage by M1) must exist
		if rid, _ := rec["request_id"].(string); rid == "" {
			t.Errorf("line %d msg=%v missing request_id (did M1's baggage fail to propagate to ctx?)", i, rec["msg"])
		} else if rid != rcRequestID {
			t.Errorf("line %d request_id=%s, want=%s", i, rid, rcRequestID)
		}
	}
}

// TestE2E_SpansFormHierarchy runs the chain against a real OTel SDK, verifying the
// child span each mw creates:
//
//  1. trace_id all == the root span's trace_id (same chain)
//  2. auth.lookup / envelope.parse's parent_span_id chains back to root
//  3. at the handler, c.Request.Context() carries the last mw's span
//
// If any mw forgets c.Request = c.Request.WithContext(ctx), its child span will be
// orphaned (parent is an invalid SpanContext), and this test will catch it.
func TestE2E_SpansFormHierarchy(t *testing.T) {
	sr := installRealTracerProvider(t)

	r := newGinTest(
		TraceContext(),
		Recover(),
		Auth(WithIdentityProvider(stubE2EIdentity{})),
		WithSourceProtocol(domain.ProtoOpenAI, domain.ModalityChat),
		Envelope(),
	)
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Status(200)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer test-token")
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	spans := sr.Ended()
	if len(spans) < 3 {
		t.Fatalf("expected >=3 spans (root + auth + envelope), got=%d", len(spans))
	}

	// index spans by name
	byName := make(map[string]sdktrace.ReadOnlySpan, len(spans))
	var rootName string
	for _, s := range spans {
		byName[s.Name()] = s
		// root looks like "POST /v1/chat/completions"
		if strings.HasPrefix(s.Name(), "POST ") {
			rootName = s.Name()
		}
	}
	if rootName == "" {
		t.Fatalf("root span not found, actual spans=%v", spanNames(spans))
	}
	root := byName[rootName]
	auth, ok := byName["auth.lookup"]
	if !ok {
		t.Fatalf("missing auth.lookup span, spans=%v", spanNames(spans))
	}
	envelope, ok := byName["envelope.parse"]
	if !ok {
		t.Fatalf("missing envelope.parse span, spans=%v", spanNames(spans))
	}

	rootTID := root.SpanContext().TraceID()
	rootSID := root.SpanContext().SpanID()

	// all spans share the same trace_id
	if auth.SpanContext().TraceID() != rootTID {
		t.Errorf("auth.lookup trace_id=%s, want=%s", auth.SpanContext().TraceID(), rootTID)
	}
	if envelope.SpanContext().TraceID() != rootTID {
		t.Errorf("envelope.parse trace_id=%s, want=%s", envelope.SpanContext().TraceID(), rootTID)
	}

	// auth.lookup's parent must be root
	if auth.Parent().SpanID() != rootSID {
		t.Errorf("auth.lookup parent span_id=%s, want root=%s (means M2 did not inherit M1's ctx)",
			auth.Parent().SpanID(), rootSID)
	}
	// envelope.parse's parent must be root (M3 sits right after M2, at the same
	// level; both are direct children of root)
	// note: envelope starts its span on c.Request.Context(); at that point
	// c.Request.Context() is the root span ctx (auth's span was already Ended and
	// restored when the auth function returned -- actually auth just writes
	// c.Request without restoring)
	// auth does not restore c.Request, so when envelope starts its span the ctx is
	// auth's ctx
	// so envelope's parent should be auth
	if envelope.Parent().SpanID() != auth.SpanContext().SpanID() {
		t.Errorf("envelope.parse parent span_id=%s, want auth=%s (means the c.Request ctx relay broke)",
			envelope.Parent().SpanID(), auth.SpanContext().SpanID())
	}

	// every span's SpanID must be distinct (only a noop tracer would collapse them into one)
	ids := map[oteltrace.SpanID]bool{}
	for _, s := range spans {
		ids[s.SpanContext().SpanID()] = true
	}
	if len(ids) != len(spans) {
		t.Errorf("some spans share the same span_id (noop tracer?), spans=%v", spanNames(spans))
	}
}

func spanNames(ss []sdktrace.ReadOnlySpan) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name()
	}
	return out
}

// =============================================================================
// stub: IdentityProvider for e2e
// =============================================================================

type stubE2EIdentity struct{}

func (stubE2EIdentity) Resolve(_ context.Context, _ *domain.Credentials) (*domain.UserIdentity, error) {
	return &domain.UserIdentity{
		AccountID:    "acc-1",
		SubAccountID: "sub-1",
		APIKeyID:     "key-1",
		Group:        "default",
	}, nil
}
