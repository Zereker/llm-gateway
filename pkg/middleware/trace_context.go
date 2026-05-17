package middleware

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// ScopeName 是 M1 root span 的 instrumentation scope 名（OTel collector 按此过滤）。
const ScopeName = "github.com/zereker/llm-gateway/pkg/middleware"

// SpanNameFormatter 把 *gin.Context 映射成 root span 名字（参考 otelgin v0.68.0）。
type SpanNameFormatter func(*gin.Context) string

// defaultSpanNameFormatter 与 otelgin 一致："{METHOD} {fullpath}"；未匹配路由时退到 "{METHOD}"。
var defaultSpanNameFormatter SpanNameFormatter = func(c *gin.Context) string {
	method := strings.ToUpper(c.Request.Method)
	if !slices.Contains([]string{
		http.MethodGet, http.MethodHead,
		http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete,
		http.MethodConnect, http.MethodOptions,
		http.MethodTrace,
	}, method) {
		method = "HTTP"
	}
	if path := c.FullPath(); path != "" {
		return method + " " + path
	}
	return method
}

// traceContextConfig 是 M1 装配配置（otelgin 同名 config 结构对位）。
type traceContextConfig struct {
	tracerProvider    oteltrace.TracerProvider
	propagators       propagation.TextMapPropagator
	spanStartOptions  []oteltrace.SpanStartOption
	spanNameFormatter SpanNameFormatter
}

// TraceContextOption 是 M1 functional option（对位 otelgin.Option）。
type TraceContextOption interface {
	apply(*traceContextConfig)
}

type traceContextOptionFunc func(*traceContextConfig)

func (f traceContextOptionFunc) apply(c *traceContextConfig) { f(c) }

// WithTraceContextTracerProvider 注入自定义 TracerProvider；nil 走 otel.GetTracerProvider()。
func WithTraceContextTracerProvider(tp oteltrace.TracerProvider) TraceContextOption {
	return traceContextOptionFunc(func(c *traceContextConfig) {
		if tp != nil {
			c.tracerProvider = tp
		}
	})
}

// WithTraceContextPropagators 注入自定义 Propagators；nil 走 otel.GetTextMapPropagator()（默认 W3C 兜底）。
func WithTraceContextPropagators(p propagation.TextMapPropagator) TraceContextOption {
	return traceContextOptionFunc(func(c *traceContextConfig) {
		if p != nil {
			c.propagators = p
		}
	})
}

// WithTraceContextSpanStartOptions 追加 root span Start 时的额外 SpanStartOption。
func WithTraceContextSpanStartOptions(opts ...oteltrace.SpanStartOption) TraceContextOption {
	return traceContextOptionFunc(func(c *traceContextConfig) {
		c.spanStartOptions = append(c.spanStartOptions, opts...)
	})
}

// WithSpanNameFormatter 自定义 root span name 推导逻辑。
func WithSpanNameFormatter(f SpanNameFormatter) TraceContextOption {
	return traceContextOptionFunc(func(c *traceContextConfig) {
		if f != nil {
			c.spanNameFormatter = f
		}
	})
}

// TraceContext 是 M1：root span + RequestContext 注入 + W3C trace context 提取。
//
// **设计参考**：opentelemetry-go-contrib / otelgin v0.68.0
// （instrumentation/github.com/gin-gonic/gin/otelgin/gin.go）—— 同款 Option 模式 +
// SpanNameFormatter + 启动期 cfg 固化 + 每请求 tracer.Start/defer span.End。
//
// **职责**（按时序）：
//
//  1. 用 cfg.propagators 从 request headers 提取上游 traceparent → ctx 里若有 valid SpanContext 续传
//  2. 没 traceparent 但有 X-Trace-Id（v0.x legacy 客户端）→ 自己构造 parent SpanContext
//  3. 都没有 → 让 tracer.Start 自动生成新 trace_id（OTel SDK 默认行为）
//  4. request_id 注入 OTel baggage（trace.CtxHandler 自动加到所有 log record，跨 service 透传）
//  5. tracer.Start("{METHOD} {route}", SpanKindServer, 初始 attrs) → root span
//  6. 构造 *domain.RequestContext，挂到 c.Request.Context() 和 *gin.Context
//  7. c.Next() 跑业务链
//  8. End 时：写 http.status_code / gen_ai.* / llm_gateway.* attrs；SetStatus；记 request.end 日志
//
// **trace_id / span_id 不存 RC 字段**——单源真相是 ctx 里的 SpanContext；
// string 形态 trace_id 用 middleware.TraceIDFromCtx 提取。
//
// **必须最先注册**（在 Recover / Auth 之前），否则 GetRequestContext 会 panic。
func TraceContext(opts ...TraceContextOption) gin.HandlerFunc {
	cfg := traceContextConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.tracerProvider == nil {
		cfg.tracerProvider = otel.GetTracerProvider()
	}
	if cfg.propagators == nil {
		cfg.propagators = defaultPropagator()
	}
	if cfg.spanNameFormatter == nil {
		cfg.spanNameFormatter = defaultSpanNameFormatter
	}
	tracer := cfg.tracerProvider.Tracer(ScopeName)

	return func(c *gin.Context) {
		savedCtx := c.Request.Context()
		defer func() { c.Request = c.Request.WithContext(savedCtx) }()

		// 1. 提取上游 traceparent（W3C）
		ctx := cfg.propagators.Extract(savedCtx, propagation.HeaderCarrier(c.Request.Header))

		// 2. legacy X-Trace-Id fallback + 兜底生成 trace_id。
		//    必须保证调 tracer.Start 之前 ctx 里有 valid SpanContext，原因有二：
		//      (a) driver=slog 时 tracer 是 noop，自己不会生成 trace_id；
		//      (b) 不论 driver 如何，trace_id 是日志关联 / outbox event 的强依赖。
		//    实 OTel SDK 时 tracer.Start 会以这个 parent 为基准生成新 span_id（trace_id 续传）。
		if !oteltrace.SpanContextFromContext(ctx).IsValid() {
			var tid oteltrace.TraceID
			if hdr := c.GetHeader(HeaderTraceID); hdr != "" {
				if parsed, err := oteltrace.TraceIDFromHex(hdr); err == nil {
					tid = parsed
				} else {
					slog.Default().Warn("M1: ignored non-W3C X-Trace-Id", "x_trace_id", hdr)
					tid = newRandTraceID()
				}
			} else {
				tid = newRandTraceID()
			}
			parentSC := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
				TraceID:    tid,
				SpanID:     newRandSpanID(),
				TraceFlags: oteltrace.FlagsSampled,
				Remote:     false,
			})
			ctx = oteltrace.ContextWithSpanContext(ctx, parentSC)
		}

		// 3. request_id 注入 baggage（gateway-only 概念，跟 trace_id 互补：trace_id 跨 service，request_id 标识单次入口）
		requestID := genRequestID()
		if m, err := baggage.NewMember("request_id", requestID); err == nil {
			if newBag, err := baggage.FromContext(ctx).SetMember(m); err == nil {
				ctx = baggage.ContextWithBaggage(ctx, newBag)
			}
		}

		// 4. 计算 span name 并开 root span
		spanName := cfg.spanNameFormatter(c)
		if spanName == "" {
			spanName = fmt.Sprintf("HTTP %s route not found", c.Request.Method)
		}

		startOpts := []oteltrace.SpanStartOption{
			oteltrace.WithSpanKind(oteltrace.SpanKindServer),
			oteltrace.WithAttributes(
				attribute.String("http.request.method", c.Request.Method),
				attribute.String("http.route", c.FullPath()),
				attribute.String("url.scheme", schemeOf(c.Request)),
				attribute.String("client.address", c.ClientIP()),
				attribute.String("user_agent.original", c.Request.UserAgent()),
				attribute.String("llm_gateway.request_id", requestID),
			),
		}
		startOpts = append(startOpts, cfg.spanStartOptions...)

		ctx, span := tracer.Start(ctx, spanName, startOpts...)
		defer span.End()

		// 5. 构造并挂 RequestContext
		rc := &domain.RequestContext{
			RequestID: requestID,
			StartTime: time.Now(),
			Ctx:       ctx,
			Extras:    make(map[string]any),
		}
		// AttachRequestContext 内部把 (rc, span ctx) 一起写回 c.Request.Context()——
		// 不要在它之后再用 bare span ctx 覆盖（会丢掉 rc value）。
		AttachRequestContext(c, rc)

		// 6. request.start 日志（debug；docs/08 §2）
		slog.DebugContext(ctx, "request.start",
			"method", c.Request.Method,
			"path", c.FullPath(),
			"request_id", requestID,
		)

		c.Next()

		// 7. End 时收尾
		statusCode := c.Writer.Status()
		elapsedMs := time.Since(rc.StartTime).Milliseconds()

		span.SetAttributes(
			attribute.Int("http.response.status_code", statusCode),
			attribute.Int64("llm_gateway.duration_ms", elapsedMs),
		)
		if rc.ModelService != nil {
			span.SetAttributes(attribute.String("gen_ai.request.model", rc.ModelService.Model))
		}
		if rc.RoutedModelService != nil {
			span.SetAttributes(attribute.String("gen_ai.response.model", rc.RoutedModelService.Model))
		}
		if rc.Endpoint != nil {
			span.SetAttributes(
				attribute.String("gen_ai.system", rc.Endpoint.Vendor),
				attribute.Int64("llm_gateway.endpoint.id", rc.Endpoint.ID),
			)
		}
		if rc.Identity.AccountID != "" {
			span.SetAttributes(attribute.String("llm_gateway.account.id", rc.Identity.AccountID))
		}
		if rc.Identity.SubAccountID != "" {
			span.SetAttributes(attribute.String("llm_gateway.sub_account.id", rc.Identity.SubAccountID))
		}
		if rc.Identity.APIKeyID != "" {
			span.SetAttributes(attribute.String("llm_gateway.api_key.id", rc.Identity.APIKeyID))
		}
		if rc.Usage != nil {
			span.SetAttributes(
				attribute.Int64("gen_ai.usage.input_tokens", rc.Usage.Input),
				attribute.Int64("gen_ai.usage.output_tokens", rc.Usage.Output),
				attribute.Int64("gen_ai.usage.total_tokens", rc.Usage.Total),
			)
			if rc.Usage.Meta.TTFTMs > 0 {
				span.SetAttributes(attribute.Int64("gen_ai.response.ttft_ms", rc.Usage.Meta.TTFTMs))
			}
		}

		// SetStatus：rc.Error 优先；否则按 HTTP 状态码（5xx → Error，4xx 默认 Unset 与 otelgin 一致）
		switch {
		case rc.Error != nil:
			span.SetAttributes(
				attribute.String("llm_gateway.error.code", rc.Error.Code),
				attribute.String("llm_gateway.error.class", rc.Error.Class.String()),
			)
			span.SetStatus(codes.Error, rc.Error.Message)
		case statusCode >= 500:
			span.SetStatus(codes.Error, http.StatusText(statusCode))
		}

		// gin.Context.Errors 同步到 span（仿 otelgin）
		if len(c.Errors) > 0 {
			for _, e := range c.Errors {
				span.RecordError(e.Err)
			}
		}

		level := slog.LevelInfo
		switch {
		case statusCode >= 500:
			level = slog.LevelError
		case statusCode >= 400:
			level = slog.LevelWarn
		}
		slog.Log(ctx, level, "request.end",
			"request_id", requestID,
			"method", c.Request.Method,
			"path", c.FullPath(),
			"status", statusCode,
			"latency_ms", elapsedMs,
		)
	}
}

// schemeOf 返回 "https"（TLS）或 "http"。
func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// defaultPropagator 拿 OTel global propagator；空 / noop 时退到 W3C TraceContext。
//
// 一次性 lazy 初始化，避免每请求 lookup。OtelTracer 装配时已经 otel.SetTextMapPropagator
// 覆盖了 global，所以装配 OTel 的进程拿到的是真 propagator；driver=slog 的进程拿到 W3C 兜底。
var (
	defaultPropagatorOnce sync.Once
	defaultPropagatorVal  propagation.TextMapPropagator
)

func defaultPropagator() propagation.TextMapPropagator {
	defaultPropagatorOnce.Do(func() {
		p := otel.GetTextMapPropagator()
		if p == nil || len(p.Fields()) == 0 {
			p = propagation.TraceContext{}
		}
		defaultPropagatorVal = p
	})
	return defaultPropagatorVal
}

// newRandTraceID 生成 W3C 标准 32-hex trace ID。
func newRandTraceID() oteltrace.TraceID {
	tid, _ := oteltrace.TraceIDFromHex(randHex(16))
	return tid
}

// newRandSpanID 生成 W3C 标准 16-hex span ID。
func newRandSpanID() oteltrace.SpanID {
	sid, _ := oteltrace.SpanIDFromHex(randHex(8))
	return sid
}

// genRequestID 生成形如 "req_<12 hex>" 的网关内部请求 ID（48 bit 随机；与 trace_id 互补）。
func genRequestID() string {
	return "req_" + randHex(6)
}

// randHex 返回 byteLen 字节随机数据的 hex 字符串（长度 = 2 * byteLen）。
// crypto/rand 失败时退到 timestamp 兜底（极少发生；让请求别 panic）。
func randHex(byteLen int) string {
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
