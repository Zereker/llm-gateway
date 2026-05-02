package middleware

import (
	"context"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// BudgetGate M4 Budget middleware 的依赖接口。
//
// 内置默认实现 alwayspass（永远放行，适合无付费体系场景）；
// 接入外部计费系统时实现自定义 BudgetGate。
type BudgetGate interface {
	Check(c context.Context, userID string) (domain.BudgetStatus, error)
}
