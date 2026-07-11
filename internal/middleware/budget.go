package middleware

import (
	"context"
	"log/slog"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/metric"
)

// BudgetGate is the dependency interface for the M4 Budget middleware.
//
// Default implementation AlwaysPassGate (always allows); implement a custom
// BudgetGate when integrating with an external billing system.
//
// Implementations MUST be safe for concurrent use.
type BudgetGate interface {
	Check(c context.Context, subAccountID string) (domain.BudgetStatus, error)
}

// BudgetOption configures the Budget middleware (same interface-Option pattern as otelgin v0.68.0).
type BudgetOption interface {
	apply(*budgetConfig)
}

type budgetOptionFunc func(*budgetConfig)

func (f budgetOptionFunc) apply(c *budgetConfig) { f(c) }

type budgetConfig struct {
	gate BudgetGate
}

// WithBudgetGate injects a BudgetGate implementation. Not passing one means
// M4 silently passes through.
func WithBudgetGate(g BudgetGate) BudgetOption {
	return budgetOptionFunc(func(c *budgetConfig) { c.gate = g })
}

// Budget is M4: calls BudgetGate to check whether the current subAccountID
// can still spend.
//
// Failure behavior:
//   - Gate returns an error → 502 / ErrUnknown / "budget check error: <err>"
//   - Status is not Active → 402 / ErrPermanent / "budget inactive"
//
// If no Gate is injected, calls c.Next() directly (equivalent to alwayspass).
func Budget(opts ...BudgetOption) gin.HandlerFunc {
	cfg := budgetConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.gate == nil {
		// pass-through fast path: doesn't even open a tracer.
		return func(c *gin.Context) { c.Next() }
	}
	tracer := otel.GetTracerProvider().Tracer(ScopeName)

	return func(c *gin.Context) {
		ctx, span := tracer.Start(c.Request.Context(), "budget.check")
		defer span.End()
		c.Request = c.Request.WithContext(ctx)

		rc := GetRequestContext(c)
		status, err := cfg.gate.Check(ctx, rc.Identity.SubAccountID)
		if err != nil {
			metric.Inc(metric.BudgetCheckTotal, "result", "error")
			slog.ErrorContext(ctx, "m4: budget check failed", "err", err)
			abortWithCode(c, 502, domain.ErrUnknown, domain.ErrCodeUpstreamError,
				"budget check unavailable")
			return
		}

		if status != domain.BudgetActive {
			metric.Inc(metric.BudgetCheckTotal, "result", "inactive")
			abortWithCode(c, 402, domain.ErrPermanent, domain.ErrCodeBudgetInactive,
				"budget inactive: "+status.String())
			return
		}

		metric.Inc(metric.BudgetCheckTotal, "result", "ok")
		c.Next()
	}
}
