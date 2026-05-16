package middleware

import (
	"context"
	"sync"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// InMemoryBudgetGate 进程内余额跟踪：按 subAccountID 维度维护 remaining_balance（USD 金额）。
//
// **适用场景**：单实例 demo / 单主账号私有部署 / 开发期联调。生产多副本部署需要外部存储
// （Redis / DB），本实现不能跨进程共享余额。
//
// **deduction 不在本组件**：BudgetGate.Check 是 pre-flight gate（请求进来时判余额），
// 真正的 cost 计算在 M10 Tracing 完成（按 usage × pricing）。本 Gate 暴露 Deduct
// 方法供外部 deducter（订阅 outbox 事件 / 定时 batch 调）回填——v0.5 这部分先不接，
// Gate 维持每个 user 配置的初始余额作为 hard cap。
//
// **配置方式**：
//   - 全局默认余额：用 NewInMemoryBudgetGate(default)，新 subAccountID 首次出现时分配
//   - 显式按 user 设置：SetBalance(subAccountID, balance)，覆盖默认
//   - 余额 ≤ 0 → BudgetInactive；> 0 → BudgetActive
//
// **零余额行为**：默认余额 0 = 所有未显式 SetBalance 的 user 都被拒绝（safe-by-default）。
// 想"无限制"用 AlwaysPassGate，不要传一个巨大的 default。
//
// Concurrent-safe（RWMutex 保护内部 map）。
type InMemoryBudgetGate struct {
	mu             sync.RWMutex
	balances       map[string]float64
	defaultBalance float64
}

// NewInMemoryBudgetGate 构造一个进程内余额 Gate。
//
// defaultBalance：未在 SetBalance 显式配过的 subAccountID 用这个值。0 = 默认拒绝（safe-by-default）。
func NewInMemoryBudgetGate(defaultBalance float64) *InMemoryBudgetGate {
	return &InMemoryBudgetGate{
		balances:       make(map[string]float64),
		defaultBalance: defaultBalance,
	}
}

// Check 实现 BudgetGate.Check：余额 > 0 返回 BudgetActive，否则 BudgetInactive。
//
// 不修改余额；纯读判断。
func (g *InMemoryBudgetGate) Check(_ context.Context, subAccountID string) (domain.BudgetStatus, error) {
	g.mu.RLock()
	balance, ok := g.balances[subAccountID]
	g.mu.RUnlock()
	if !ok {
		balance = g.defaultBalance
	}
	if balance > 0 {
		return domain.BudgetActive, nil
	}
	return domain.BudgetInactive, nil
}

// SetBalance 设置 / 覆盖某 user 的余额。admin 调用 / 测试 seed。
func (g *InMemoryBudgetGate) SetBalance(subAccountID string, balance float64) {
	g.mu.Lock()
	g.balances[subAccountID] = balance
	g.mu.Unlock()
}

// Deduct 从 subAccountID 余额扣 cost；不存在的 subAccountID 按 defaultBalance 起算。
//
// 返回扣完后的剩余余额；负数说明这次请求把余额打穿了（M10 完成后调用，事后扣账，
// 当前请求不会被拒；下次 Check 才生效）。
//
// 没接订阅 outbox 事件之前不会被自动调用——v0.5 留为公共 API 让外部脚本 / 测试可以模拟。
func (g *InMemoryBudgetGate) Deduct(subAccountID string, cost float64) float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	balance, ok := g.balances[subAccountID]
	if !ok {
		balance = g.defaultBalance
	}
	balance -= cost
	g.balances[subAccountID] = balance
	return balance
}

// GetBalance 读当前余额（admin / 测试）。
func (g *InMemoryBudgetGate) GetBalance(subAccountID string) float64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	balance, ok := g.balances[subAccountID]
	if !ok {
		return g.defaultBalance
	}
	return balance
}

// 编译期断言：InMemoryBudgetGate 满足 BudgetGate。
var _ BudgetGate = (*InMemoryBudgetGate)(nil)
