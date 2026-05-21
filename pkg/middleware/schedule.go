package middleware

import (
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

// Schedule 是 M7 middleware——thin adapter：把 gin / RC 转 dispatch.Input，
// 跑 dispatcher.Dispatch，再把 dispatch.Outcome 映射回 RC + HTTP。
//
// **职责**：
//   1. 前置检查 rc.Envelope / rc.ModelChain（M3/M5 必须先跑过）
//   2. 注入 content log enrichment（Invoker hook 通过 ctx 拿请求元信息；docs/05 §2）
//   3. 构造 dispatch.Input（envelope / identity / modelChain / handlers / 客户端 header override）
//   4. 调 dispatcher.Dispatch 跑业务编排
//   5. metric: scheduling_duration_seconds
//   6. 从 outcome 写回 RC（RoutedModelService / Usage / Error / SchedulingDecision）
//   7. 把 Outcome 翻译成 HTTP 错误响应（成功路径已 stream，啥也不做）
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

		// 构造 dispatch.Input —— RC → typed input 单向投影（dispatch 不接触 RC）
		in := dispatch.Input{
			Envelope:           rc.Envelope,
			Identity:           rc.Identity,
			ModelChain:         rc.ModelChain,
			Handlers:           HandlersFrom(rc),
			AttemptCapOverride: c.GetHeader(HeaderGatewayMaxAttempts),
		}

		// metric: scheduling_duration_seconds（docs/08 §3）
		start := time.Now()
		out := d.Dispatch(ctx, c.Writer, in)

		attempts := 0
		if out.Decision != nil {
			attempts = len(out.Decision.Attempts)
		}
		metric.Observe(metric.SchedulingDurationSeconds, time.Since(start).Seconds(),
			"model", rc.ModelService.Model,
			"attempts", strconv.Itoa(attempts),
		)

		// dispatch.Outcome → RC 单向回写（dispatch 不直接动 RC）
		applyOutcomeToRC(rc, out)

		// 失败路径翻译成 HTTP（成功路径已通过 c.Writer stream 完）
		if out.Result == dispatch.OutcomeStreamed {
			return
		}
		abortByOutcome(c, out)
	}
}

// applyOutcomeToRC 把 dispatch 产出的字段映射回 RC（dispatch 已解耦 RC，
// 所有副作用集中在这里）。
func applyOutcomeToRC(rc *domain.RequestContext, out dispatch.Outcome) {
	if out.RoutedModel != nil {
		rc.RoutedModelService = out.RoutedModel
	}
	if out.Usage != nil {
		rc.Usage = out.Usage
	}
	if out.Error != nil {
		rc.Error = out.Error
	}
	if out.Decision != nil {
		rc.SchedulingDecision = out.Decision
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
