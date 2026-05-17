package middleware

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/trace"
	"github.com/zereker/llm-gateway/pkg/usage"
)

// TracingOption 配置 Tracing middleware（otelgin v0.68.0 同款 interface-Option）。
type TracingOption interface {
	apply(*tracingConfig)
}

type tracingOptionFunc func(*tracingConfig)

func (f tracingOptionFunc) apply(c *tracingConfig) { f(c) }

type tracingConfig struct {
	outbox         usage.OutboxPublisher
	tracer         trace.Tracer // 业务 trace.Tracer（写 scheduling_decision），≠ OTel tracer
	tracerProvider oteltrace.TracerProvider
}

// WithUsageOutbox 注入 Usage Event outbox publisher。
//
// 不传 = M10 不发 usage event（仅记 metric / trace）。
func WithUsageOutbox(o usage.OutboxPublisher) TracingOption {
	return tracingOptionFunc(func(c *tracingConfig) { c.outbox = o })
}

// WithTracer 注入 业务 trace.Tracer（写 scheduling_decision 日志）；不传 = 跳过。
//
// 注意：跟 OTel TracerProvider 是两回事——这里的 trace.Tracer 是网关内部审计 trace
// （pkg/trace），用于把 SchedulingDecision 写到日志 / outbox。
func WithTracer(t trace.Tracer) TracingOption {
	return tracingOptionFunc(func(c *tracingConfig) { c.tracer = t })
}

// WithTracingTracerProvider 注入 OTel TracerProvider；nil 时启动期退到 otel.GetTracerProvider()。
func WithTracingTracerProvider(tp oteltrace.TracerProvider) TracingOption {
	return tracingOptionFunc(func(c *tracingConfig) {
		if tp != nil {
			c.tracerProvider = tp
		}
	})
}

// Tracing 是 M10：聚合 metric + 发计量事件 + 写 SchedulingDecision trace。
// 在 c.Next() 之后执行（defer 模式）。
//
// 发布失败不影响业务返回（best-effort）。
// 用 context.Background()（带超时）发 Outbox，避免 client 已断开时
// 还是要把 usage 落出（docs/05 §3）。
func Tracing(opts ...TracingOption) gin.HandlerFunc {
	cfg := tracingConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.tracerProvider == nil {
		cfg.tracerProvider = otel.GetTracerProvider()
	}
	otelTracer := cfg.tracerProvider.Tracer(ScopeName)

	return func(c *gin.Context) {
		c.Next()

		rc := GetRequestContext(c)

		ctx, span := otelTracer.Start(rc.Ctx, "tracing.commit")
		defer span.End()
		rc.Ctx = ctx

		now := time.Now().UTC()
		elapsed := now.Sub(rc.StartTime)

		// HTTP latency metric（docs/08 §3）
		// labels: method / route / status / model / routed_model
		var model, routedModel string
		if rc.ModelService != nil {
			model = rc.ModelService.Model
		}
		if rc.RoutedModelService != nil {
			routedModel = rc.RoutedModelService.Model
		} else {
			routedModel = model
		}
		metric.Observe(metric.HTTPRequestDurationSeconds, elapsed.Seconds(),
			"method", c.Request.Method,
			"route", c.FullPath(),
			"status", strconv.Itoa(c.Writer.Status()),
			"model", model,
			"routed_model", routedModel,
		)

		// llm_gateway_http_requests_total counter
		errClass := ""
		if rc.Error != nil {
			errClass = rc.Error.Class.String()
		}
		metric.Inc(metric.HTTPRequestsTotal,
			"method", c.Request.Method,
			"route", c.FullPath(),
			"status", strconv.Itoa(c.Writer.Status()),
			"error_class", errClass,
		)

		if rc.Usage != nil {
			fillUsageMeta(rc, now, elapsed.Milliseconds())
			// usage tokens metric
			metric.Add(metric.UsageTokensTotal, float64(rc.Usage.Input),
				"model", model, "routed_model", routedModel,
				"vendor", rc.Usage.Meta.Vendor, "direction", "input")
			metric.Add(metric.UsageTokensTotal, float64(rc.Usage.Output),
				"model", model, "routed_model", routedModel,
				"vendor", rc.Usage.Meta.Vendor, "direction", "output")
		}

		// Usage Event outbox
		if rc.Usage != nil && cfg.outbox != nil {
			publishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			evt := buildUsageEvent(rc)
			payload, err := json.Marshal(evt)
			if err == nil {
				key := rc.Identity.AccountID
				if key == "" {
					key = rc.RequestID
				}
				result := "ok"
				if err := cfg.outbox.Publish(publishCtx, &usage.OutboxEvent{
					Payload: payload,
					Key:     key,
				}); err != nil {
					result = "error"
				}
				metric.Inc(metric.UsagePublishTotal, "backend", "outbox", "result", result)
			}
		}

		if rc.SchedulingDecision != nil && cfg.tracer != nil {
			cfg.tracer.Log(rc.Ctx, "scheduling_decision", rc.SchedulingDecision)
		}
	}
}

// fillUsageMeta 把 rc 全链路状态聚合到 rc.Usage.Meta。
//
// 按 docs/05 §4 字段来源表。RoutedModelService 优先；fallback 时
// usage.meta.Model = 实际成功的 model（不是请求的 model）。
func fillUsageMeta(rc *domain.RequestContext, endTime time.Time, totalLatencyMs int64) {
	m := &rc.Usage.Meta

	m.StartTime = rc.StartTime
	m.EndTime = endTime
	m.TotalLatency = totalLatencyMs
	// TTFTMs 由 upstream/forward.go 在首字节流出时回写

	m.RequestID = rc.RequestID
	m.TraceID = TraceIDFromCtx(rc.Ctx)

	m.AccountID = rc.Identity.AccountID
	m.SubAccountID = rc.Identity.SubAccountID
	m.APIKeyID = rc.Identity.APIKeyID

	// 路由后的 model（docs/05 §4）；fallback 时不同于请求 model
	routed := rc.RoutedModelService
	if routed == nil {
		routed = rc.ModelService
	}
	if routed != nil {
		m.Model = routed.Model
		m.ServiceID = routed.ServiceID
	}

	if rc.Endpoint != nil {
		m.Vendor = rc.Endpoint.Vendor
		m.EndpointID = strconv.FormatInt(rc.Endpoint.ID, 10)
	}
}

// buildUsageEvent 按 docs/05 §5 + docs/08 §5 的 envelope 形态打包 Usage Event。
func buildUsageEvent(rc *domain.RequestContext) usage.UsageEvent {
	return usage.UsageEvent{
		SchemaVersion: usage.SchemaVersionV1,
		EventID:       newEventID(),
		RequestID:     rc.RequestID,
		TraceID:       TraceIDFromCtx(rc.Ctx),
		Usage:         *rc.Usage,
		CreatedAt:     time.Now().UTC(),
	}
}

// newEventID 简单 UUID 形态。
func newEventID() string {
	return "evt_" + randHex(8)
}
