package trace

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/baggage"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// CtxHandler 包一层 slog.Handler，从 ctx 自动抽 trace 信息加到 log record。
//
// **抽取来源**：
//
//  1. **OTel SpanContext**（从 W3C traceparent 来）→ trace_id / span_id
//  2. **OTel Baggage** → 全部 members 当 attr（业务侧用 baggage.SetMember 加
//     sub_account_id / request_id 等会自动出现在 log）
//
// **使用约定**：所有需要 trace 关联的 log 都用 `slog.InfoContext(ctx, ...)`（带 ctx 的变体），
// 不要用 `slog.Info` —— 后者拿不到 ctx，handler 提取不到 trace 信息。
//
// **装配**（main.go 里）：
//
//	base := slog.NewJSONHandler(os.Stderr, nil)
//	slog.SetDefault(slog.New(trace.NewCtxHandler(base)))
//
// 之后任何代码 `slog.InfoContext(ctx, "msg", ...)` 自动带 trace_id / span_id /
// 已注入 baggage 的字段（sub_account_id 等）。
//
// **没装 OTel TracerProvider 时也工作**：M1 TraceContext middleware 自己用
// W3C propagator 注入 SpanContext，与是否启用 OTel 上报无关；CtxHandler 只看 ctx，
// 不依赖 TracerProvider。
//
// **baggage 安全**：本 handler 把 baggage 全 member 都暴露到 log。生产里**不要
// 往 baggage 写敏感数据**（API key / 密码 / PII）——baggage 设计就是"明文跨 service
// 传播"，本就不适合存敏感数据。
type CtxHandler struct {
	inner slog.Handler
}

// NewCtxHandler 包装一个底层 slog.Handler；inner=nil 时退到 JSONHandler(stderr)。
func NewCtxHandler(inner slog.Handler) *CtxHandler {
	if inner == nil {
		inner = slog.NewJSONHandler(os.Stderr, nil)
	}
	return &CtxHandler{inner: inner}
}

// Enabled 透传给内层 handler。
func (h *CtxHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.inner.Enabled(ctx, lvl)
}

// Handle 在 record 上追加 ctx 抽出的属性，再调内层 handler。
//
// 注意 record 顺序：trace_id / span_id 先于 baggage member——后追加的会出现在
// JSON 输出靠后位置（slog 实现保序）。
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

// WithAttrs / WithGroup 透传：返回包装后的 handler 保持 ctx-aware 能力。
func (h *CtxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &CtxHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *CtxHandler) WithGroup(name string) slog.Handler {
	return &CtxHandler{inner: h.inner.WithGroup(name)}
}
