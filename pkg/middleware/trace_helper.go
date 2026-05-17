package middleware

import (
	"context"

	oteltrace "go.opentelemetry.io/otel/trace"
)

// TraceIDFromCtx 从 ctx 拿 W3C trace_id 字符串（32 hex）；缺失返空串。
//
// 数据源：M1 TraceContext middleware 注入的 oteltrace.SpanContext。
//
// **使用场景**：序列化层需要 string 形态的 trace_id（写 JSON 响应、写 outbox 事件、
// 当 logger label 等）。**不要**把返回值缓存到 RC 字段后到处传——直接 call 即可，
// SpanContext 在 ctx 里是单源真相。
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
