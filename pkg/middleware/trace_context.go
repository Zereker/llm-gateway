package middleware

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// TraceContext 是 M1：构造 *domain.RequestContext，挂到 *gin.Context。
//
// 必须最先注册（链中第一个 middleware）；否则 GetRequestContext 会 panic。
//
// **W3C trace context 集成**（v1.0+）：
//
//  1. 提取上游 traceparent header（W3C 标准格式 `00-<32hex traceID>-<16hex spanID>-<flags>`）
//     用 OTel propagator 解析；有效 → 续传，rc.TraceID 跟上游一致
//  2. 没有 traceparent 但有 X-Trace-Id（legacy 兼容）：
//     - 32-hex 合法 → 当 trace_id 用，新生成 span_id
//     - 其它格式 → 完全忽略，记 warning log
//  3. 都没有 → 生成全新 W3C 标准 ID
//
// SpanContext 永远是**新生成**span_id 的（trace_id 续上游或新生成）——它代表
// "网关本次请求"这个 span，下游 / OtelTracer 子 span 用它做 parent。
//
// **行为**：
//   - 把 SpanContext{trace_id, span_id} 注入 c.Request.Context() 作为 rc.Ctx；
//     下游 OtelTracer.StartSpan 自动续 parent（无需手工传 ID）
//   - request_id 注入 OTel baggage；trace.CtxHandler 自动从 ctx 把 trace_id /
//     span_id / baggage 全字段（user_id 由 M2 Auth 后写入）加到所有 log record
//   - 用 slog.InfoContext(rc.Ctx, ...) 输出日志即可，不用手工传 trace_id 字段
//
// **trace_id / span_id 不存 RC 字段**——单源真相是 ctx 里的 SpanContext；要 string
// 形态的 trace_id 用 middleware.TraceIDFromCtx 提取（span_id 没人需要 string 形态）。
//
// 客户端 X-Trace-Id 兼容性留着——老调用方（v0.x 客户端）切到 W3C 之前不会断。
func TraceContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		// 1. 优先尝试 W3C traceparent
		prop := getPropagator()
		ctx = prop.Extract(ctx, propagation.HeaderCarrier(c.Request.Header))
		parentSC := oteltrace.SpanContextFromContext(ctx)

		var traceID oteltrace.TraceID
		if parentSC.IsValid() {
			traceID = parentSC.TraceID()
		} else if hdr := c.GetHeader(HeaderTraceID); hdr != "" {
			// 2. fallback X-Trace-Id（v0.x 客户端兼容）
			if tid, err := oteltrace.TraceIDFromHex(hdr); err == nil {
				traceID = tid
			} else {
				// 不合法 W3C 32-hex → 生成新的；把原值记进 logger 方便对账
				traceID = newRandTraceID()
				slog.Default().Warn("M1: ignored non-W3C X-Trace-Id",
					"x_trace_id", hdr, "generated_trace_id", traceID.String())
			}
		} else {
			// 3. 全新生成
			traceID = newRandTraceID()
		}

		// rc.SpanID 永远是网关本次请求 fresh 的 span_id
		spanID := newRandSpanID()

		// 用我们的 (trace_id, span_id) 构造 SpanContext 注入 ctx；
		// 下游 OtelTracer.StartSpan 会以此为 parent 创建子 span。
		newSC := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     spanID,
			TraceFlags: parentSC.TraceFlags() | oteltrace.FlagsSampled, // 默认 sampled，尊重上游 flags
			TraceState: parentSC.TraceState(),
			Remote:     false,
		})
		ctx = oteltrace.ContextWithSpanContext(ctx, newSC)

		// request_id 注入 OTel baggage，让 trace.CtxHandler 自动加到所有 log record。
		// 跨 service 也跟着走（baggage 标准会经 W3C baggage header 透传到上游）。
		requestID := genRequestID()
		if member, err := baggage.NewMember("request_id", requestID); err == nil {
			if newBag, err := baggage.FromContext(ctx).SetMember(member); err == nil {
				ctx = baggage.ContextWithBaggage(ctx, newBag)
			}
		}

		rc := &domain.RequestContext{
			RequestID: requestID,
			StartTime: time.Now(),
			Ctx:       ctx,
			Extras:    make(map[string]any),
		}

		AttachRequestContext(c, rc)
		c.Next()
	}
}

// propagator 缓存 OTel global propagator；启动期一次性设好（init），避免每请求 lookup。
var (
	propagatorOnce sync.Once
	propagator     propagation.TextMapPropagator
)

// getPropagator 拿 OTel global propagator；首次调用时如果 global 没设过，注入
// W3C TraceContext 当默认值（让"没用 OtelTracer 但 client 发了 traceparent"也工作）。
//
// OtelTracer 装配时会调 otel.SetTextMapPropagator 覆盖；所以装配 OTel 的进程
// 用真 OTel propagator，没装配的进程用我们这里的 default。
func getPropagator() propagation.TextMapPropagator {
	propagatorOnce.Do(func() {
		propagator = otel.GetTextMapPropagator()
		// otel.GetTextMapPropagator 默认返回 noop；用 W3C TraceContext 兜底
		if _, ok := propagator.(propagation.TextMapPropagator); !ok || isNoopPropagator(propagator) {
			propagator = propagation.TraceContext{}
		}
	})
	return propagator
}

// isNoopPropagator 判断当前 propagator 是不是 noop。OTel SDK 的 noop 实现
// `Extract` / `Inject` 都是空操作；用 Fields() 长度=0 判定（noop 不声明任何 header field）。
func isNoopPropagator(p propagation.TextMapPropagator) bool {
	if p == nil {
		return true
	}

	return len(p.Fields()) == 0
}

// newRandTraceID 用我们的 randHex 生成 32 hex → 转 OTel TraceID 类型。
func newRandTraceID() oteltrace.TraceID {
	tid, _ := oteltrace.TraceIDFromHex(genTraceID())
	return tid
}

// newRandSpanID 用我们的 randHex 生成 16 hex → 转 OTel SpanID 类型。
func newRandSpanID() oteltrace.SpanID {
	sid, _ := oteltrace.SpanIDFromHex(genSpanID())
	return sid
}

// genTraceID 生成 W3C trace context 标准的 trace ID：16 字节 / 128 bit / 32 hex 字符。
//
// 跟 OpenTelemetry trace.TraceID 的字节宽度对齐，保证 OTel propagator 提取 / 注入
// 双向兼容（client 发的 traceparent → 我们解析；我们生成的 → 跨 service 续传）。
//
// 不带前缀（W3C 规定不允许）；对应 OtelTracer 用 SDK 自生成的 ID 时也是这种格式。
func genTraceID() string {
	return randHex(16)
}

// genSpanID 生成 W3C trace context 标准的 span ID：8 字节 / 64 bit / 16 hex 字符。
//
// 这是网关本次请求的 "root span ID"：所有 OtelTracer.StartSpan 出来的子 span 都
// 用它作 parent；外部下游 service 看到的 traceparent 里的 span_id 也是它。
func genSpanID() string {
	return randHex(8)
}

// genRequestID 生成形如 "req_<12 hex>" 的请求 ID（48 bit 随机）。
//
// **不是** OpenTelemetry 概念——这是网关内部的 per-call audit ID，主要用于：
//   - 应用日志关联同一次进入网关的请求（vs trace_id 跨 service）
//   - 客户端报障时给"我那次调用是 req_xxx"方便定位
//
// 同请求内唯一；跨请求 48 bit 在百万 QPS 量级下冲突概率可忽略。
func genRequestID() string {
	return "req_" + randHex(6)
}

// randHex 返回 byteLen 字节随机数据的 hex 字符串（长度 = 2 * byteLen）。
//
// crypto/rand 失败时退到 timestamp 兜底（极少发生，但避免 panic 让请求失败）。
func randHex(byteLen int) string {
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
