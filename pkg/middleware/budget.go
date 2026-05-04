package middleware

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/metric"
)

// BudgetGate M4 Budget middleware 的依赖接口。
//
// 内置默认实现 alwayspass（永远放行，适合无付费体系场景）；
// 接入外部计费系统时实现自定义 BudgetGate。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
type BudgetGate interface {
	Check(c context.Context, userID string) (domain.BudgetStatus, error)
}

// BudgetDeps M4 Budget middleware 的依赖。
//
// Gate 为 nil 时 handler 静默 pass-through——开发期或不需付费体系的部署可省略；
// 生产部署接入计费系统时显式注入实现。
type BudgetDeps struct {
	Gate BudgetGate
}

// Budget 是 M4：调 BudgetGate 判断当前 userID 是否仍可消费。
//
// 失败行为：
//   - Gate 报错 → 502 / ErrUnknown / "budget check error: <err>"（计费系统挂；不应放过）
//   - 状态非 Active → 402 / ErrPermanent / "budget inactive"
//
// 成功后写 rc.BudgetStatus；下游 middleware 不再 query。
//
// Gate 为 nil 时直接 c.Next()（视同 alwayspass）。
func Budget(deps BudgetDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps.Gate == nil {
			c.Next()
			return
		}
		rc := GetRequestContext(c)

		status, err := deps.Gate.Check(rc.Ctx, rc.Identity.UserID)
		if err != nil {
			metric.Inc(metric.BudgetCheckTotal, "result", "error")
			abort(c, 502, domain.ErrUnknown, "budget check error: "+err.Error())
			return
		}
		rc.BudgetStatus = status
		if status != domain.BudgetActive {
			metric.Inc(metric.BudgetCheckTotal, "result", "inactive")
			abort(c, 402, domain.ErrPermanent, "budget inactive: "+status.String())
			return
		}
		metric.Inc(metric.BudgetCheckTotal, "result", "ok")
		c.Next()
	}
}
