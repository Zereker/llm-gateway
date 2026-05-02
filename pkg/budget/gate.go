// Package budget 定义 M4 Budget middleware 的依赖：预算 / 配额检查（gate）。
//
// 默认实现 alwayspass（永远放行）见 step 2；接入外部计费系统可自定义实现。
package budget

import (
	"context"

	"github.com/zereker-labs/ai-gateway/pkg/ctx"
)

// Gate M4 Budget middleware 的依赖接口。
type Gate interface {
	Check(c context.Context, userID string) (ctx.BudgetStatus, error)
}
