package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zereker/llm-gateway/internal/requeststate"
	"go.opentelemetry.io/otel"

	"github.com/zereker/llm-gateway/internal/contentlog"
	"github.com/zereker/llm-gateway/internal/dispatch"
	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/metric"
)

// MaxFallbackModels is the maximum number of models allowed in the
// X-Gateway-Fallback-Models header (docs/03 §5).
//
// Parsing is done in M5 (the ModelService middleware); dispatch.Dispatcher
// consumes rc.ModelChain directly.
const MaxFallbackModels = 3

// Schedule is the M7 middleware — a thin adapter: converts gin / RC into
// dispatch.Input, runs dispatcher.Dispatch, then maps dispatch.Outcome back
// onto RC + HTTP.
//
// **Responsibilities**:
//  1. Pre-flight checks on rc.Envelope / rc.ModelChain (M3/M5 must have already run)
//  2. Injects content log enrichment (the Invoker hook gets request metadata via ctx; docs/05 §2)
//  3. Builds dispatch.Input (envelope / identity / modelChain / handlers / client header overrides)
//  4. Calls dispatcher.Dispatch to run the business orchestration
//  5. Metric: scheduling_duration_seconds
//  6. Writes fields back onto RC from the outcome (RoutedModelService / Usage / Error / SchedulingDecision)
//  7. Translates the Outcome into an HTTP error response (the success path has already streamed, nothing to do)
//
// **Does NOT do**: retry / fallback / verdict decisions / reserve / charge /
// TTFT — all orchestrated inside internal/dispatch.
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

		// content log enrichment (the Logger gets request metadata via ctx)
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

		// Builds dispatch.Input — a one-way RC → typed input projection (dispatch never touches RC)
		in := dispatch.Input{
			Envelope:           rc.Envelope,
			Identity:           rc.Identity,
			ModelChain:         rc.ModelChain,
			Handlers:           HandlersFrom(rc),
			AttemptCapOverride: c.GetHeader(HeaderGatewayMaxAttempts),
			SessionKey:         c.GetHeader(HeaderGatewaySession),
		}

		// metric: scheduling_duration_seconds (docs/08 §3)
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

		// dispatch.Outcome → RC one-way write-back (dispatch never touches RC directly)
		applyOutcomeToRC(rc, out)

		// Translate the failure path into HTTP (the success path has already
		// finished streaming via c.Writer)
		if out.Result == dispatch.OutcomeStreamed {
			return
		}

		abortByOutcome(c, out)
	}
}

// applyOutcomeToRC maps fields produced by dispatch back onto RC (dispatch is
// decoupled from RC; all side effects are centralized here).
func applyOutcomeToRC(rc *requeststate.State, out dispatch.Outcome) {
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

// abortByOutcome translates dispatch.Outcome into an HTTP error.
func abortByOutcome(c *gin.Context, out dispatch.Outcome) {
	cls := dispatchClassToDomain(out.Class)
	code := errCodeFromDispatchClass(out.Class, out.Result)
	abortWithDetails(c, out.HTTPCode, cls, code, out.Reason, map[string]any{
		"result": out.Result.String(),
	})
}

// dispatchClassToDomain converts dispatch.Class → domain.ErrorClass.
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

// errCodeFromDispatchClass picks a domain.ErrCode string.
//
// Prefers Result (OutcomeInvalid always maps to invalid_request; NoEndpoint
// maps to no_endpoint_available), otherwise falls back based on Class.
func errCodeFromDispatchClass(c dispatch.Class, r dispatch.OutcomeResult) string {
	switch r {
	case dispatch.OutcomeInvalid:
		return domain.ErrCodeInvalidRequest
	case dispatch.OutcomeNoEndpoint:
		return domain.ErrCodeNoEndpointAvailable
	case dispatch.OutcomeDepFail:
		return domain.ErrCodeDependencyUnavailable
	case dispatch.OutcomeClientAbort:
		return domain.ErrCodeClientClosedRequest
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
