package middleware

import (
	"context"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"

	"github.com/zereker/llm-gateway/pkg/contentlog"
	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
)

// MaxFallbackModels X-Gateway-Fallback-Models header 允许的最多 model 数（docs/03 §5）。
//
// 解析在 M5（ModelService middleware）完成；dispatch.Dispatcher 直接消费 rc.ModelChain。
const MaxFallbackModels = 3

// EndpointReader M7 用：按 (model, group) 拉候选 endpoints。
//
// **保留为 middleware-owned 接口**：cmd/gateway 的 selectorAdapter 通过这个 port
// 拿数据，repo SQL 实现通过 cmd/gateway/middleware_adapters.go 的 adaptEndpoints
// 桥接。PR3 时把接口归属移到 pkg/selector 包。
type EndpointReader interface {
	ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error)
}

// Schedule 是 M7 middleware——thin adapter：把 gin 上下文转给 dispatch.Dispatcher。
//
// **职责**：
//   1. 前置检查 rc.Envelope / rc.ModelChain（M3/M5 必须先跑过）
//   2. 注入 content log enrichment（Invoker hook 通过 ctx 拿请求元信息；docs/05 §2）
//   3. 写 X-Gateway-Max-Attempts header 到 rc.Extras 供 HeaderAttemptCap 读
//   4. 调 dispatcher.Dispatch 跑业务编排
//   5. metric: scheduling_duration_seconds
//   6. 把 Outcome 翻译成 HTTP 错误响应（成功路径已 stream，啥也不做）
//
// **不做**：retry / fallback / verdict 决策 / reserve / charge / TTFT——全部在
// pkg/dispatch 内编排。
func Schedule(d *dispatch.Dispatcher) gin.HandlerFunc {
	if d == nil {
		panic("middleware.Schedule: dispatch.Dispatcher required")
	}
	tracer := otel.GetTracerProvider().Tracer(ScopeName)

	return func(c *gin.Context) {
		ctx, span := tracer.Start(c.Request.Context(), "selector.dispatch")
		defer span.End()

		rc := GetRequestContext(c)
		if rc.Envelope == nil || rc.ModelService == nil || len(rc.ModelChain) == 0 {
			c.Request = c.Request.WithContext(ctx)
			abortWithCode(c, 500, domain.ErrUnknown, domain.ErrCodeInternalError,
				"internal: M3/M5 did not run before M7")
			return
		}

		// content log enrichment（Logger 通过 ctx 拿请求元信息）
		ctx = contentlog.EnrichCtx(ctx, contentlog.RequestEnrich{
			RequestID:    rc.RequestID,
			TraceID:      TraceIDFromCtx(ctx),
			AccountID:    rc.Identity.AccountID,
			APIKeyID:     rc.Identity.APIKeyID,
			SubAccountID: rc.Identity.SubAccountID,
			Model:        rc.ModelService.Model,
			Protocol:     rc.Envelope.SourceProtocol.String(),
			Modality:     rc.Envelope.Modality.String(),
		})
		c.Request = c.Request.WithContext(ctx)

		// 把 max-attempts header 透给 dispatch.HeaderAttemptCap
		if rc.Extras == nil {
			rc.Extras = make(map[string]any, 1)
		}
		if h := c.GetHeader(HeaderGatewayMaxAttempts); h != "" {
			rc.Extras[dispatch.HeaderKey] = h
		}

		// metric: scheduling_duration_seconds（docs/08 §3）
		start := time.Now()
		out := d.Dispatch(ctx, c.Writer, rc)

		attempts := 0
		if out.Decision != nil {
			attempts = len(out.Decision.Attempts)
		}
		metric.Observe(metric.SchedulingDurationSeconds, time.Since(start).Seconds(),
			"model", rc.ModelService.Model,
			"attempts", strconv.Itoa(attempts),
		)

		// 失败路径翻译成 HTTP（成功路径已通过 c.Writer stream 完）
		if out.Result == dispatch.OutcomeStreamed {
			if out.StreamErr != nil {
				rc.Error = &domain.AdapterError{
					Class:   domain.ErrTransient,
					Code:    domain.ErrCodeUpstreamError,
					Message: "stream: " + out.StreamErr.Error(),
				}
			}
			return
		}
		abortByOutcome(c, out)
	}
}

// abortByOutcome 把 dispatch.Outcome 翻译成 HTTP 错误。
func abortByOutcome(c *gin.Context, out dispatch.Outcome) {
	cls := dispatchClassToDomain(out.Class)
	code := errCodeFromDispatchClass(out.Class, out.Result)
	abortWithDetails(c, out.HTTPCode, cls, code, out.Reason, map[string]any{
		"result": out.Result.String(),
	})
}

// dispatchClassToDomain dispatch.Class → domain.ErrorClass。
func dispatchClassToDomain(c dispatch.Class) domain.ErrorClass {
	switch c {
	case dispatch.ClassTransient:
		return domain.ErrTransient
	case dispatch.ClassCapacity:
		return domain.ErrRateLimit
	case dispatch.ClassPermanent:
		return domain.ErrPermanent
	case dispatch.ClassInvalid:
		return domain.ErrInvalid
	default:
		return domain.ErrUnknown
	}
}

// errCodeFromDispatchClass 选 domain.ErrCode 字符串。
//
// 优先看 Result（OutcomeInvalid 一律走 invalid_request；NoEndpoint 走 no_endpoint_available），
// 否则按 Class 兜底。
func errCodeFromDispatchClass(c dispatch.Class, r dispatch.OutcomeResult) string {
	switch r {
	case dispatch.OutcomeInvalid:
		return domain.ErrCodeInvalidRequest
	case dispatch.OutcomeNoEndpoint:
		return domain.ErrCodeNoEndpointAvailable
	case dispatch.OutcomeDepFail:
		return domain.ErrCodeDependencyUnavailable
	}
	switch c {
	case dispatch.ClassCapacity:
		return domain.ErrCodeRateLimitExceeded
	case dispatch.ClassPermanent:
		return domain.ErrCodeUpstreamError
	case dispatch.ClassInvalid:
		return domain.ErrCodeInvalidRequest
	case dispatch.ClassTransient:
		return domain.ErrCodeUpstreamError
	}
	return domain.ErrCodeInternalError
}
