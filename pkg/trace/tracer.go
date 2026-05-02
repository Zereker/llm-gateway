// Package trace 定义结构化 trace 接口（OpenTelemetry / slog 等可插拔实现）。
//
// 默认实现见 step 2+。
package trace

import "context"

// Tracer 结构化日志 / span 接口。
//
// Tracer 实现 MUST be safe for concurrent use（多 gin handler goroutine 同时 Log / StartSpan）。
type Tracer interface {
	// Log 写一条结构化日志（带 trace_id 等上下文）
	Log(c context.Context, name string, payload any)

	// StartSpan 开启一个 span（可选 OTel 集成）
	StartSpan(c context.Context, name string) (context.Context, Span)
}

// Span 一个 trace span。
//
// Span 实例只在创建它的 goroutine 内使用（与 OTel SpanProcessor 约定一致）；
// 实现无需自加锁。
type Span interface {
	SetAttribute(key string, value any)
	End()
}
