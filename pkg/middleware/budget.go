package middleware

import (
	"context"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
)

// BudgetGate M4 Budget middleware 的依赖接口。
//
// 默认实现 AlwaysPassGate（永远放行）；接入外部计费系统时实现自定义 BudgetGate。
//
// Implementations MUST be safe for concurrent use。
type BudgetGate interface {
	Check(c context.Context, subAccountID string) (domain.BudgetStatus, error)
}

// BudgetOption 配置 Budget middleware（otelgin v0.68.0 同款 interface-Option）。
type BudgetOption interface {
	apply(*budgetConfig)
}

type budgetOptionFunc func(*budgetConfig)

func (f budgetOptionFunc) apply(c *budgetConfig) { f(c) }

type budgetConfig struct {
	gate BudgetGate
}

// WithBudgetGate 注入 BudgetGate 实现。不传 = M4 静默 pass-through。
func WithBudgetGate(g BudgetGate) BudgetOption {
	return budgetOptionFunc(func(c *budgetConfig) { c.gate = g })
}

// Budget 是 M4：调 BudgetGate 判断当前 subAccountID 是否仍可消费。
//
// 失败行为：
//   - Gate 报错 → 502 / ErrUnknown / "budget check error: <err>"
//   - 状态非 Active → 402 / ErrPermanent / "budget inactive"
//
// 未注入 Gate 时直接 c.Next()（视同 alwayspass）。
func Budget(opts ...BudgetOption) gin.HandlerFunc {
	cfg := budgetConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.gate == nil {
		// pass-through 快路径：连 tracer 都不开。
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
			abortWithCode(c, 502, domain.ErrUnknown, domain.ErrCodeUpstreamError,
				"budget check error: "+err.Error())
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
