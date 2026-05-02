package trace

import (
	"context"
	"log/slog"
)

// SlogTracer 是 Tracer 的零依赖默认实现，把每条 Log 写到 slog。
//
// Span 走 NoOp（slog 本身不带 span 概念）；如需真正的 span 树，
// 在 v1.0 用 pkg/trace/otel 替换。
type SlogTracer struct {
	Logger *slog.Logger
}

// NewSlogTracer 用给定 logger 构造；nil 时退到 slog.Default()。
func NewSlogTracer(logger *slog.Logger) *SlogTracer {
	if logger == nil {
		logger = slog.Default()
	}
	return &SlogTracer{Logger: logger}
}

// Log 实现 Tracer.Log：把 name 作为 message、payload 作为 attr 落 slog。
func (t *SlogTracer) Log(c context.Context, name string, payload any) {
	t.Logger.LogAttrs(c, slog.LevelInfo, name, slog.Any("payload", payload))
}

// StartSpan 实现 Tracer.StartSpan：返回 NoOp Span，不记录 span 树。
func (t *SlogTracer) StartSpan(c context.Context, name string) (context.Context, Span) {
	return c, noopSpan{}
}

// noopSpan SetAttribute / End 都不做任何事。
type noopSpan struct{}

func (noopSpan) SetAttribute(key string, value any) {}
func (noopSpan) End()                                {}
