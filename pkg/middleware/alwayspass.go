package middleware

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// AlwaysPassGate 是 BudgetGate 的零依赖默认实现：永远返回 BudgetActive。
//
// 用于无付费 / 无配额体系的场景（开发、单租户内部网关）。
// 接入外部计费系统时实现自定义 BudgetGate。
//
// 零值即可用：var gate AlwaysPassGate；不需要构造函数。
type AlwaysPassGate struct{}

// Check 实现 BudgetGate.Check：永远 BudgetActive、永远 nil。
func (AlwaysPassGate) Check(_ context.Context, _ string) (domain.BudgetStatus, error) {
	return domain.BudgetActive, nil
}

// 编译期断言：AlwaysPassGate 满足 BudgetGate。
var _ BudgetGate = AlwaysPassGate{}
