package middleware

import (
	"context"

	"go.opentelemetry.io/otel"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// middlewareTracer 全 package 共享的 OTel tracer instance。
//
// 用 OTel global TracerProvider；如果 main.go 没装 OTel（driver=slog）则得到
// noop tracer，Start/End 是空操作，0 开销。所以这个机制对 SlogTracer 用户透明。
//
// instrumentation name 用 `pkg/middleware`——OTel 推荐用代码包路径作 instrumentation
// 名，方便 collector 端按 lib 维度 filter。
var middlewareTracer = otel.Tracer("github.com/zereker/llm-gateway/pkg/middleware")

// startSpan 开启一个 OTel span，返回 (新 ctx, end 函数)。
//
// **标准 ctx-in / ctx-out pattern**（跟 stdlib 对齐）。Caller 责任：
//  1. 把返回的 ctx **写回 rc.Ctx**（让下游 middleware 续 parent）
//  2. defer end()
//
// 调用模板（middleware 第一行）：
//
//	ctx, end := startSpan(rc.Ctx, "llm-gateway.<name>")
//	defer end()
//	rc.Ctx = ctx
//
// **不要忘记 `rc.Ctx = ctx`**——忘了会导致下个 middleware 的 span 拿不到本 span 当 parent，
// trace 树断成两截。Go vet 不会警告这个；只能靠习惯 / code review。
//
// 需要在 span 上 SetAttribute / RecordError 时调 middlewareTracer.Start 直接拿 span ref，
// 跳过本 helper（保持本 helper 极简——大多数场景只要 ctx + end）。
func startSpan(ctx context.Context, name string) (context.Context, func()) {
	ctx, span := middlewareTracer.Start(ctx, name)
	return ctx, func() { span.End() }
}

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
