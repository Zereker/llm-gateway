// Package budget 定义 M4 Budget middleware 的依赖：预算 / 配额检查（gate）。
//
// 内置默认实现 alwayspass（永远放行，适合无付费体系场景）；
// 接入外部计费系统时实现自定义 Gate。
package budget

import (
	"context"

	"github.com/zereker-labs/ai-gateway/pkg/ctx"
)

// Gate M4 Budget middleware 的依赖接口。
type Gate interface {
	Check(c context.Context, userID string) (ctx.BudgetStatus, error)
}
