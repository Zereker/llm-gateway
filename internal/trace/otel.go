// Package trace's OpenTelemetry tracer implementation (v1.0 G2.7).
//
// **Wiring**:
//
//	tp, err := trace.NewOtelProvider(ctx, "my-gateway", "http://otel-collector:4317")
//	if err != nil { ... }
//	defer tp.Shutdown(ctx)
//	tracer := trace.NewOtelTracer(tp)
//
// Feed the tracer into middleware.TracingDeps.Tracer to replace SlogTracer.
package trace

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// NewOtelProvider builds an OTLP gRPC exporter + TracerProvider.
//
// service: written to the OTel resource.service.name; "llm-gateway" is recommended.
// endpoint: the OTLP collector's gRPC address (e.g. "otel-collector.observability:4317");
// leave empty to fall back to the OTel SDK's default environment variable
// (OTEL_EXPORTER_OTLP_ENDPOINT).
//
// The returned *TracerProvider must have Shutdown called before process exit so
// buffered spans get flushed.
func NewOtelProvider(ctx context.Context, service, endpoint string) (*sdktrace.TracerProvider, error) {
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithInsecure(), // works out of the box; edit this function directly for mTLS in production
	}
	if endpoint != "" {
		opts = append(opts, otlptracegrpc.WithEndpoint(endpoint))
	}
	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otel exporter: %w", err)
	}

	res, err := sdkresource.New(ctx,
		sdkresource.WithAttributes(semconv.ServiceName(service)),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	// Set the global propagator to W3C TraceContext + Baggage: this lets the M1
	// TraceContext middleware extract the traceparent / baggage headers.
	//
	// **Security warning — do not wrap the upstream HTTP client with an
	// otelhttp-style transport that auto-injects the propagator**: baggage carries
	// internal tenant identifiers such as sub_account_id / request_id, and
	// auto-injection would send them as a `baggage:` header to OpenAI / Anthropic /
	// Gemini (a cross-organization leak). The current invoker uses its own
	// http.Client and does not inject — keep it that way. If upstream calls need
	// tracing, inject only TraceContext (traceparent) and strip Baggage.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp, nil
}

// OtelTracer builds a real span tree using go.opentelemetry.io/otel.
//
// **Log() behavior**: OTel has no "log" concept; this implementation records each
// Log call as a zero-duration event: it adds an event on the current active span
// (if any); with no active span, it is dropped (the OTel SDK does not support
// free-floating events).
//
// **StartSpan**: opens a span via tracer.Start; returns an OtelSpan so that
// SetAttribute / End route to the OTel SDK.
//
// **On trace_id correlation**: the M1 TraceContext middleware already uses the
// OTel propagator to parse the W3C traceparent header
// (internal/middleware/trace_context.go) and injects the SpanContext into
// c.Request.Context(); this tracer's StartSpan(ctx, ...) automatically creates a
// child span with the current span as parent, so the trace_id matches the entry
// root span — the trace_id in the gateway logs is the same trace_id the OTel
// collector sees.
type OtelTracer struct {
	tracer oteltrace.Tracer
}

// NewOtelTracer builds a tracer from the given TracerProvider (typically the one
// returned by NewOtelProvider).
func NewOtelTracer(tp oteltrace.TracerProvider) *OtelTracer {
	return &OtelTracer{tracer: tp.Tracer("llm-gateway")}
}

// Log adds an event on the current span; silently ignored when there is no span.
//
// Strategy for converting payload to an attribute: try fmt.Sprintf("%v", payload)
// and store it as a string. For complex structures (e.g. *domain.Usage) where more
// detail is needed, tag them individually on the span via SetAttribute instead.
func (t *OtelTracer) Log(c context.Context, name string, payload any) {
	span := oteltrace.SpanFromContext(c)
	if !span.IsRecording() {
		return
	}
	attrs := []attribute.KeyValue{}
	if payload != nil {
		attrs = append(attrs, attribute.String("payload", fmt.Sprintf("%v", payload)))
	}
	span.AddEvent(name, oteltrace.WithAttributes(attrs...))
}

// StartSpan opens an OTel span; the returned ctx has the span embedded, and
// SetAttribute/End route to the OTel SDK.
func (t *OtelTracer) StartSpan(c context.Context, name string) (context.Context, Span) {
	ctx, span := t.tracer.Start(c, name)
	return ctx, &otelSpan{span: span}
}

// otelSpan adapts the trace.Span interface to an OTel span.
type otelSpan struct {
	span oteltrace.Span
}

// SetAttribute implements trace.Span.SetAttribute.
//
// Dispatch by value type: string / int / int64 / float64 / bool are built via the
// corresponding attribute.KeyValue constructor; other types fall back to
// fmt.Sprintf as a string.
func (s *otelSpan) SetAttribute(key string, value any) {
	if s.span == nil {
		return
	}
	switch v := value.(type) {
	case string:
		s.span.SetAttributes(attribute.String(key, v))
	case int:
		s.span.SetAttributes(attribute.Int(key, v))
	case int64:
		s.span.SetAttributes(attribute.Int64(key, v))
	case float64:
		s.span.SetAttributes(attribute.Float64(key, v))
	case bool:
		s.span.SetAttributes(attribute.Bool(key, v))
	default:
		s.span.SetAttributes(attribute.String(key, fmt.Sprintf("%v", v)))
	}
}

// End implements trace.Span.End.
func (s *otelSpan) End() {
	if s.span != nil {
		s.span.End()
	}
}

// Compile-time assertions.
var _ Tracer = (*OtelTracer)(nil)
var _ Span = (*otelSpan)(nil)
