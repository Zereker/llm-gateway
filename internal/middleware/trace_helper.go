package middleware

import (
	"context"

	oteltrace "go.opentelemetry.io/otel/trace"
)

// TraceIDFromCtx gets the W3C trace_id string (32 hex) from ctx; returns an
// empty string if absent.
//
// Data source: the oteltrace.SpanContext injected by the M1 TraceContext middleware.
//
// **Use case**: the serialization layer needs the string form of trace_id
// (writing JSON responses, writing outbox events, as a logger label, etc.).
// **Do not** cache the return value into an RC field and pass it around —
// just call this directly; the SpanContext in ctx is the single source of truth.
func TraceIDFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}

	sc := oteltrace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}

	return sc.TraceID().String()
}
