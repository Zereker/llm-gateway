package trace

import (
	"context"
	"log/slog"
)

// SlogTracer is Tracer's zero-dependency default implementation; it writes each
// Log call to slog.
//
// Span is a NoOp (slog itself has no span concept); replace with internal/trace/otel
// in v1.0 if a real span tree is needed.
type SlogTracer struct {
	Logger *slog.Logger
}

// NewSlogTracer builds a tracer from the given logger; falls back to
// slog.Default() when nil.
func NewSlogTracer(logger *slog.Logger) *SlogTracer {
	if logger == nil {
		logger = slog.Default()
	}
	return &SlogTracer{Logger: logger}
}

// Log implements Tracer.Log: writes name as the message and payload as an attr to slog.
func (t *SlogTracer) Log(c context.Context, name string, payload any) {
	t.Logger.LogAttrs(c, slog.LevelInfo, name, slog.Any("payload", payload))
}

// StartSpan implements Tracer.StartSpan: returns a NoOp Span and records no span tree.
func (t *SlogTracer) StartSpan(c context.Context, name string) (context.Context, Span) {
	return c, noopSpan{}
}

// noopSpan's SetAttribute / End do nothing.
type noopSpan struct{}

func (noopSpan) SetAttribute(key string, value any) {}
func (noopSpan) End()                               {}
