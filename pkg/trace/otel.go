// Package trace's OpenTelemetry tracer 实现（v1.0 G2.7）。
//
// **装配方式**：
//
//	tp, err := trace.NewOtelProvider(ctx, "my-gateway", "http://otel-collector:4317")
//	if err != nil { ... }
//	defer tp.Shutdown(ctx)
//	tracer := trace.NewOtelTracer(tp)
//
// 把 tracer 喂给 middleware.TracingDeps.Tracer 替换 SlogTracer。
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

// NewOtelProvider 构造 OTLP gRPC exporter + TracerProvider。
//
// service：写到 OTel resource.service.name；推荐 "ai-gateway"。
// endpoint：OTLP collector 的 gRPC 地址（如 "otel-collector.observability:4317"）；
// 留空时走 OTel SDK 默认环境变量（OTEL_EXPORTER_OTLP_ENDPOINT）。
//
// 返回的 *TracerProvider 必须在进程退出前调 Shutdown 让缓冲 span flush 出去。
func NewOtelProvider(ctx context.Context, service, endpoint string) (*sdktrace.TracerProvider, error) {
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithInsecure(), // 开箱即用；生产改 mTLS 自己改本函数
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
	// 全局 propagator 设为 W3C TraceContext + Baggage：让 M1 TraceContext middleware
	// 提取 traceparent / baggage header；下游 outgoing HTTP（如果 adapter 用 OTel
	// instrumented client 也会自动注入 traceparent 到 upstream request）。
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp, nil
}

// OtelTracer 用 go.opentelemetry.io/otel 做真 span 树。
//
// **Log() 行为**：OTel 没有 "log" 概念；本实现把 Log call 作为 0 持续时间的 event：
// 在当前 active span（如果有）上加一个 event；没 active span 时丢弃（OTel SDK 不
// 支持 free-floating event）。
//
// **StartSpan**：用 tracer.Start 开 span；返回 OtelSpan 让 SetAttribute / End 路由
// 到 OTel SDK。
//
// **关于 trace_id 联动**：M1 TraceContext middleware 已用 OTel propagator 解析
// W3C traceparent header（pkg/middleware/trace_context.go），把 SpanContext 注入
// rc.Ctx；本 tracer 的 StartSpan(rc.Ctx, ...) 自动以 rc.SpanID 为 parent 创建
// 子 span，trace_id 跟 rc.TraceID 一致——gateway 日志里的 trace_id 和 OTel
// collector 看到的 trace_id 是同一个。
type OtelTracer struct {
	tracer oteltrace.Tracer
}

// NewOtelTracer 用给定 TracerProvider 构造（typically 拿 NewOtelProvider 返回的）。
func NewOtelTracer(tp oteltrace.TracerProvider) *OtelTracer {
	return &OtelTracer{tracer: tp.Tracer("ai-gateway")}
}

// Log 在当前 span 上加 event；无 span 时静默忽略。
//
// payload 转 attribute 的策略：试 fmt.Sprintf("%v", payload) 当 string 存。
// 复杂结构（如 *domain.Usage）要看到细节就用 SetAttribute 在 span 上单独标。
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

// StartSpan 开启 OTel span；返回的 ctx 内嵌 span，SetAttribute/End 路由到 OTel SDK。
func (t *OtelTracer) StartSpan(c context.Context, name string) (context.Context, Span) {
	ctx, span := t.tracer.Start(c, name)
	return ctx, &otelSpan{span: span}
}

// otelSpan 把 trace.Span 接口适配到 OTel span。
type otelSpan struct {
	span oteltrace.Span
}

// SetAttribute 实现 trace.Span.SetAttribute。
//
// value 类型分发：string / int / int64 / float64 / bool 走对应 attribute.KeyValue
// 构造；其它类型 fmt.Sprintf 当 string。
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

// End 实现 trace.Span.End。
func (s *otelSpan) End() {
	if s.span != nil {
		s.span.End()
	}
}

// 编译期断言。
var _ Tracer = (*OtelTracer)(nil)
var _ Span = (*otelSpan)(nil)
