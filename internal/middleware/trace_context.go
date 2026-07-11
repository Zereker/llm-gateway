package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/zereker/llm-gateway/internal/requeststate"
)

// ScopeName is the instrumentation scope name for M1's root span (the OTel
// collector filters by this).
const ScopeName = "github.com/zereker/llm-gateway/internal/middleware"

// defaultSpanNameFormatter matches otelgin: "{METHOD} {fullpath}"; falls back
// to "{METHOD}" when the route isn't matched.
func defaultSpanNameFormatter(c *gin.Context) string {
	method := strings.ToUpper(c.Request.Method)
	if !slices.Contains([]string{
		http.MethodGet, http.MethodHead,
		http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete,
		http.MethodConnect, http.MethodOptions,
		http.MethodTrace,
	}, method) {
		method = "HTTP"
	}
	if path := c.FullPath(); path != "" {
		return method + " " + path
	}
	return method
}

// TraceContext is M1: root span + RequestContext injection + W3C trace
// context extraction.
//
// **Design reference**: opentelemetry-go-contrib / otelgin v0.68.0
// (instrumentation/github.com/gin-gonic/gin/otelgin/gin.go) — set up once at
// startup + per-request tracer.Start/defer span.End.
//
// **Responsibilities** (in order):
//
//  1. Extract the upstream traceparent from request headers → continues if ctx already has a valid SpanContext
//  2. No traceparent → constructs its own parent SpanContext, generating a fallback trace_id
//  3. Injects request_id into OTel baggage (trace.CtxHandler automatically adds it to every log record, propagated across services)
//  4. tracer.Start("{METHOD} {route}", SpanKindServer, initial attrs) → root span
//  5. Constructs requeststate.State and attaches it to c.Request.Context()
//  6. c.Next() runs the business chain
//  7. On End: writes http.status_code / gen_ai.* / llm_gateway.* attrs; SetStatus; logs request.end
//
// **trace_id / span_id are not stored as RC fields** — the single source of
// truth is the SpanContext in ctx; the string form of trace_id is extracted
// via middleware.TraceIDFromCtx.
//
// **Must be registered first** (before Recover / Auth), otherwise
// GetRequestContext will panic.
func TraceContext() gin.HandlerFunc {
	propagators := defaultPropagator()
	tracer := otel.GetTracerProvider().Tracer(ScopeName)

	return func(c *gin.Context) {
		savedCtx := c.Request.Context()
		defer func() { c.Request = c.Request.WithContext(savedCtx) }()

		// 1. Extract the upstream traceparent (W3C)
		ctx := propagators.Extract(savedCtx, propagation.HeaderCarrier(c.Request.Header))

		// 2. Fallback trace_id generation.
		//    Must guarantee ctx has a valid SpanContext before calling
		//    tracer.Start, for two reasons:
		//      (a) with driver=slog the tracer is a noop and won't generate a
		//          trace_id on its own;
		//      (b) regardless of driver, trace_id is a hard dependency for log
		//          correlation / outbox events.
		//    With a real OTel SDK, tracer.Start generates a new span_id based
		//    on this parent (trace_id carries through).
		if !oteltrace.SpanContextFromContext(ctx).IsValid() {
			parentSC := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
				TraceID:    newRandTraceID(),
				SpanID:     newRandSpanID(),
				TraceFlags: oteltrace.FlagsSampled,
				Remote:     false,
			})
			ctx = oteltrace.ContextWithSpanContext(ctx, parentSC)
		}

		// 3. Inject request_id into baggage (a gateway-only concept,
		//    complementary to trace_id: trace_id spans services, request_id
		//    identifies a single entry point)
		requestID := genRequestID()
		if m, err := baggage.NewMember("request_id", requestID); err == nil {
			if newBag, err := baggage.FromContext(ctx).SetMember(m); err == nil {
				ctx = baggage.ContextWithBaggage(ctx, newBag)
			}
		}

		// 4. Compute the span name and open the root span
		spanName := defaultSpanNameFormatter(c)
		if spanName == "" {
			spanName = fmt.Sprintf("HTTP %s route not found", c.Request.Method)
		}

		ctx, span := tracer.Start(ctx, spanName,
			oteltrace.WithSpanKind(oteltrace.SpanKindServer),
			oteltrace.WithAttributes(
				attribute.String("http.request.method", c.Request.Method),
				attribute.String("http.route", c.FullPath()),
				attribute.String("url.scheme", schemeOf(c.Request)),
				attribute.String("client.address", c.ClientIP()),
				attribute.String("user_agent.original", c.Request.UserAgent()),
				attribute.String("llm_gateway.request_id", requestID),
			),
		)
		defer span.End()

		// 5. Construct and attach RequestContext
		rc := &requeststate.State{
			RequestID: requestID,
			StartTime: time.Now(),
		}
		// AttachRequestContext writes the current ctx + rc value back onto
		// c.Request.Context() together. Downstream middleware should always
		// get ctx from c.Request.Context() afterward, never bypass this.
		c.Request = c.Request.WithContext(ctx)
		AttachRequestContext(c, rc)

		// 6. request.start log (debug; docs/08 §2)
		slog.DebugContext(ctx, "request.start",
			"method", c.Request.Method,
			"path", c.FullPath(),
			"request_id", requestID,
		)

		c.Next()

		// 7. Finalize on End
		statusCode := c.Writer.Status()
		elapsedMs := time.Since(rc.StartTime).Milliseconds()

		span.SetAttributes(
			attribute.Int("http.response.status_code", statusCode),
			attribute.Int64("llm_gateway.duration_ms", elapsedMs),
		)
		if rc.ModelService != nil {
			span.SetAttributes(attribute.String("gen_ai.request.model", rc.ModelService.Model))
		}
		if rc.RoutedModelService != nil {
			span.SetAttributes(attribute.String("gen_ai.response.model", rc.RoutedModelService.Model))
		}
		if rc.Endpoint != nil {
			span.SetAttributes(
				attribute.String("gen_ai.system", rc.Endpoint.Vendor),
				attribute.Int64("llm_gateway.endpoint.id", rc.Endpoint.ID),
			)
		}
		if rc.Identity.AccountID != "" {
			span.SetAttributes(attribute.String("llm_gateway.account.id", rc.Identity.AccountID))
		}
		if rc.Identity.SubAccountID != "" {
			span.SetAttributes(attribute.String("llm_gateway.sub_account.id", rc.Identity.SubAccountID))
		}
		if rc.Identity.APIKeyID != "" {
			span.SetAttributes(attribute.String("llm_gateway.api_key.id", rc.Identity.APIKeyID))
		}
		if rc.Usage != nil {
			span.SetAttributes(
				attribute.Int64("gen_ai.usage.input_tokens", rc.Usage.Input),
				attribute.Int64("gen_ai.usage.output_tokens", rc.Usage.Output),
				attribute.Int64("gen_ai.usage.total_tokens", rc.Usage.Total),
			)
			if rc.Usage.Meta.TTFTMs > 0 {
				span.SetAttributes(attribute.Int64("gen_ai.response.ttft_ms", rc.Usage.Meta.TTFTMs))
			}
		}

		// SetStatus: rc.Error takes priority; otherwise based on HTTP status
		// code (5xx → Error, 4xx defaults to Unset, consistent with otelgin)
		switch {
		case rc.Error != nil:
			span.SetAttributes(
				attribute.String("llm_gateway.error.code", rc.Error.Code),
				attribute.String("llm_gateway.error.class", rc.Error.Class.String()),
			)
			span.SetStatus(codes.Error, rc.Error.Message)
		case statusCode >= 500:
			span.SetStatus(codes.Error, http.StatusText(statusCode))
		}

		// Sync gin.Context.Errors to the span (mirrors otelgin)
		if len(c.Errors) > 0 {
			for _, e := range c.Errors {
				span.RecordError(e.Err)
			}
		}

		level := slog.LevelInfo
		switch {
		case statusCode >= 500:
			level = slog.LevelError
		case statusCode >= 400:
			level = slog.LevelWarn
		}
		slog.Log(ctx, level, "request.end",
			"request_id", requestID,
			"method", c.Request.Method,
			"path", c.FullPath(),
			"status", statusCode,
			"latency_ms", elapsedMs,
		)
	}
}

// schemeOf returns "https" (TLS) or "http".
func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// defaultPropagator gets the OTel global propagator; falls back to W3C
// TraceContext when empty / noop.
//
// One-time lazy init, avoiding a per-request lookup. When OtelTracer is wired
// up it has already called otel.SetTextMapPropagator to override the global,
// so a process with OTel wired up gets the real propagator; a process with
// driver=slog gets the W3C fallback.
var (
	defaultPropagatorOnce sync.Once
	defaultPropagatorVal  propagation.TextMapPropagator
)

func defaultPropagator() propagation.TextMapPropagator {
	defaultPropagatorOnce.Do(func() {
		p := otel.GetTextMapPropagator()
		if p == nil || len(p.Fields()) == 0 {
			p = propagation.TraceContext{}
		}
		defaultPropagatorVal = p
	})
	return defaultPropagatorVal
}

// newRandTraceID generates a W3C-standard 32-hex trace ID.
func newRandTraceID() oteltrace.TraceID {
	tid, _ := oteltrace.TraceIDFromHex(randHex(16))
	return tid
}

// newRandSpanID generates a W3C-standard 16-hex span ID.
func newRandSpanID() oteltrace.SpanID {
	sid, _ := oteltrace.SpanIDFromHex(randHex(8))
	return sid
}

// genRequestID generates a gateway-internal request ID of the form
// "req_<12 hex>" (48-bit random; complementary to trace_id).
func genRequestID() string {
	return "req_" + randHex(6)
}

// randHex returns the hex string of byteLen bytes of random data (length =
// 2 * byteLen). Falls back to a timestamp when crypto/rand fails (rare;
// keeps the request from panicking).
func randHex(byteLen int) string {
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
