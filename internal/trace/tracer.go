// Package trace defines the structured trace interface (pluggable implementations
// such as OpenTelemetry / slog).
//
// See step 2+ for the default implementation.
package trace

import "context"

// Tracer is the structured logging / span interface.
//
// Tracer implementations MUST be safe for concurrent use (multiple gin handler
// goroutines calling Log / StartSpan at the same time).
type Tracer interface {
	// Log writes one structured log entry (with trace_id and other context).
	Log(c context.Context, name string, payload any)

	// StartSpan opens a span (optional OTel integration).
	StartSpan(c context.Context, name string) (context.Context, Span)
}

// Span is a single trace span.
//
// Span instances are only used within the goroutine that created them (consistent
// with the OTel SpanProcessor convention); implementations need not add their own
// locking.
type Span interface {
	SetAttribute(key string, value any)
	End()
}
