package trace

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/baggage"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// CtxHandler wraps a slog.Handler, automatically extracting trace info from ctx
// and adding it to the log record.
//
// **Extraction sources**:
//
//  1. **OTel SpanContext** (from W3C traceparent) → trace_id / span_id
//  2. **OTel Baggage** → all members become attrs (business code uses
//     baggage.SetMember to add sub_account_id / request_id etc., which then
//     automatically appear in the log)
//
// **Usage convention**: all logs that need trace correlation must use
// `slog.InfoContext(ctx, ...)` (the ctx-carrying variant), not `slog.Info` — the
// latter has no access to ctx, so the handler cannot extract trace info from it.
//
// **Wiring** (in main.go):
//
//	base := slog.NewJSONHandler(os.Stderr, nil)
//	slog.SetDefault(slog.New(trace.NewCtxHandler(base)))
//
// After this, any `slog.InfoContext(ctx, "msg", ...)` call automatically carries
// trace_id / span_id / injected baggage fields (sub_account_id etc.).
//
// **Also works without an OTel TracerProvider installed**: the M1 TraceContext
// middleware injects the SpanContext itself via the W3C propagator, independent of
// whether OTel reporting is enabled; CtxHandler only looks at ctx and does not
// depend on the TracerProvider.
//
// **Baggage security**: this handler exposes all baggage members to the log.
// **Do not write sensitive data to baggage in production** (API keys / passwords /
// PII) — baggage is designed to propagate "in plaintext across services," which
// makes it inherently unsuitable for sensitive data.
type CtxHandler struct {
	inner slog.Handler
}

// NewCtxHandler wraps an underlying slog.Handler; falls back to
// JSONHandler(stderr) when inner is nil.
func NewCtxHandler(inner slog.Handler) *CtxHandler {
	if inner == nil {
		inner = slog.NewJSONHandler(os.Stderr, nil)
	}

	return &CtxHandler{inner: inner}
}

// Enabled delegates to the inner handler.
func (h *CtxHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.inner.Enabled(ctx, lvl)
}

// Handle appends attributes extracted from ctx onto the record, then calls the
// inner handler.
//
// Note the record ordering: trace_id / span_id come before the baggage members —
// attributes appended later appear later in the JSON output (slog preserves order).
func (h *CtxHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx == nil {
		return h.inner.Handle(ctx, r)
	}

	sc := oteltrace.SpanContextFromContext(ctx)
	if sc.HasTraceID() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()))
	}

	if sc.HasSpanID() {
		r.AddAttrs(slog.String("span_id", sc.SpanID().String()))
	}

	bag := baggage.FromContext(ctx)
	for _, m := range bag.Members() {
		r.AddAttrs(slog.String(m.Key(), m.Value()))
	}

	return h.inner.Handle(ctx, r)
}

// WithAttrs / WithGroup delegate through: return a wrapped handler that keeps the
// ctx-aware behavior.
func (h *CtxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &CtxHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *CtxHandler) WithGroup(name string) slog.Handler {
	return &CtxHandler{inner: h.inner.WithGroup(name)}
}
