package middleware

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zereker/llm-gateway/internal/requeststate"
	"go.opentelemetry.io/otel"

	"github.com/zereker/llm-gateway/internal/metric"
	"github.com/zereker/llm-gateway/internal/usage"
)

// UsageOutbox is the port for M10 metering-event publishing — middleware-owned.
//
// Implementers (internal/usage.FileOutbox / KafkaOutbox / AsyncKafkaOutbox /
// DualWriteOutbox) write code for their own domain and happen to satisfy this
// port. usage.OutboxEvent is a value type, kept in the usage package.
type UsageOutbox interface {
	Publish(ctx context.Context, evt *usage.OutboxEvent) error
}

// AuditTracer is the port for M10's internal audit trace — middleware-owned.
// Deliberately narrowed to a single Log method (ISP); internal/trace.Tracer
// actually also has StartSpan, but middleware currently only uses Log.
//
// Don't confuse this with the OTel TracerProvider: that's distributed
// tracing; this is the internal audit channel that writes SchedulingDecision
// to logs / async events (implemented by internal/trace.SlogTracer / OtelTracer).
type AuditTracer interface {
	Log(ctx context.Context, name string, payload any)
}

// TracingOption configures the Tracing middleware (same interface-Option pattern as otelgin v0.68.0).
type TracingOption interface {
	apply(*tracingConfig)
}

type tracingOptionFunc func(*tracingConfig)

func (f tracingOptionFunc) apply(c *tracingConfig) { f(c) }

type tracingConfig struct {
	outbox UsageOutbox
	tracer AuditTracer // business audit trace (writes scheduling_decision), ≠ OTel tracer
}

// WithUsageOutbox injects a Usage Event outbox publisher.
//
// Not passing one means M10 does not emit usage events (only records metrics / trace).
func WithUsageOutbox(o UsageOutbox) TracingOption {
	return tracingOptionFunc(func(c *tracingConfig) { c.outbox = o })
}

// WithTracer injects an audit AuditTracer (writes scheduling_decision logs);
// not passing one means it's skipped.
//
// Note: this is a different thing from the OTel TracerProvider — this is the
// gateway's internal audit channel (implemented by internal/trace.SlogTracer /
// OtelTracer), used to write SchedulingDecision to logs / outbox.
func WithTracer(t AuditTracer) TracingOption {
	return tracingOptionFunc(func(c *tracingConfig) { c.tracer = t })
}

// Tracing is M10: aggregates metrics + emits metering events + writes the
// SchedulingDecision trace. Runs after c.Next() (defer pattern).
//
// Publish failures don't affect the business response (best-effort).
// Uses context.Background() (with a timeout) to publish to the Outbox, so
// usage still gets flushed out even if the client has already disconnected
// (docs/05 §3).
func Tracing(opts ...TracingOption) gin.HandlerFunc {
	cfg := tracingConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	otelTracer := otel.GetTracerProvider().Tracer(ScopeName)

	return func(c *gin.Context) {
		c.Next()

		ctx, span := otelTracer.Start(c.Request.Context(), "tracing.commit")
		defer span.End()
		c.Request = c.Request.WithContext(ctx)

		rc := GetRequestContext(c)
		now := time.Now().UTC()
		elapsed := now.Sub(rc.StartTime)

		// HTTP latency metric (docs/08 §3)
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
			fillUsageMeta(rc, ctx, now, elapsed.Milliseconds())
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
			cfg.tracer.Log(ctx, "scheduling_decision", rc.SchedulingDecision)
		}
	}
}

// fillUsageMeta aggregates rc's end-to-end state into rc.Usage.Meta.
//
// Follows the field source table in docs/05 §4. RoutedModelService takes
// priority; on fallback, usage.meta.Model = the model that actually
// succeeded (not the requested model).
func fillUsageMeta(rc *requeststate.State, ctx context.Context, endTime time.Time, totalLatencyMs int64) {
	m := &rc.Usage.Meta

	m.StartTime = rc.StartTime
	m.EndTime = endTime
	m.TotalLatency = totalLatencyMs
	// TTFTMs is written back by upstream/forward.go when the first byte streams out

	m.RequestID = rc.RequestID
	m.TraceID = TraceIDFromCtx(ctx)

	m.AccountID = rc.Identity.AccountID
	m.SubAccountID = rc.Identity.SubAccountID
	m.APIKeyID = rc.Identity.APIKeyID

	// The routed model (docs/05 §4); differs from the requested model on fallback
	routed := rc.RoutedModelService
	if routed == nil {
		routed = rc.ModelService
	}
	if routed != nil {
		m.Model = routed.Model
		m.ServiceID = routed.ServiceID
		m.ModelServiceID = routed.ID
		m.ServiceUpdateTime = routed.UpdatedAt
	}

	if rc.Endpoint != nil {
		m.Vendor = rc.Endpoint.Vendor
		m.EndpointID = strconv.FormatInt(rc.Endpoint.ID, 10)
	}
}

// buildUsageEvent packages a Usage Event following the envelope shape in
// docs/05 §5 + docs/08 §5.
//
// request_id / trace_id are not at the envelope top level — the authoritative
// values live in rc.Usage.Meta (written by fillUsageMeta).
func buildUsageEvent(rc *requeststate.State) usage.UsageEvent {
	return usage.UsageEvent{
		SchemaVersion: usage.SchemaVersionV1,
		EventID:       newEventID(),
		Usage:         *rc.Usage,
		CreatedAt:     time.Now().UTC(),
	}
}

// newEventID generates the billing dedup key for a usage event. 128-bit
// random (UUID-strength): consumers deduplicate on event_id, and a birthday
// collision would silently drop a legitimate event — under-billing. 64 bits
// (the previous width) reaches 50% collision odds around 2^32 events, which
// a long-running gateway can plausibly hit.
func newEventID() string {
	return "evt_" + randHex(16)
}
