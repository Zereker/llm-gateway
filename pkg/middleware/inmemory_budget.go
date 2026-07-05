package middleware

import (
	"context"
	"sync"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// InMemoryBudgetGate tracks balance in-process: maintains remaining_balance
// (a USD amount) keyed by subAccountID.
//
// **Applicable scenarios**: single-instance demo / single-tenant private
// deployment / dev-time integration testing. Production multi-replica
// deployments need external storage (Redis / DB); this implementation cannot
// share balance across processes.
//
// **Deduction is not part of this component**: BudgetGate.Check is a
// pre-flight gate (checks balance when a request comes in); the actual cost
// calculation happens in M10 Tracing (usage × pricing). This Gate exposes a
// Deduct method for an external deducter (subscribing to outbox events / a
// scheduled batch job) to feed back — not wired up yet in v0.5, the Gate
// currently just holds each user's configured initial balance as a hard cap.
//
// **Configuration**:
//   - Global default balance: use NewInMemoryBudgetGate(default); assigned the
//     first time a new subAccountID appears
//   - Explicit per-user setting: SetBalance(subAccountID, balance), overrides the default
//   - Balance ≤ 0 → BudgetInactive; > 0 → BudgetActive
//
// **Zero-balance behavior**: default balance of 0 = every user without an
// explicit SetBalance is rejected (safe-by-default). Use AlwaysPassGate for
// "unlimited"; don't pass a huge default instead.
//
// Concurrent-safe (RWMutex protects the internal map).
type InMemoryBudgetGate struct {
	mu             sync.RWMutex
	balances       map[string]float64
	defaultBalance float64
}

// NewInMemoryBudgetGate constructs an in-process balance Gate.
//
// defaultBalance: used for any subAccountID not explicitly configured via
// SetBalance. 0 = reject by default (safe-by-default).
func NewInMemoryBudgetGate(defaultBalance float64) *InMemoryBudgetGate {
	return &InMemoryBudgetGate{
		balances:       make(map[string]float64),
		defaultBalance: defaultBalance,
	}
}

// Check implements BudgetGate.Check: returns BudgetActive if balance > 0,
// otherwise BudgetInactive.
//
// Does not modify the balance; read-only check.
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

// SetBalance sets / overrides a user's balance. For ops / test seeding.
func (g *InMemoryBudgetGate) SetBalance(subAccountID string, balance float64) {
	g.mu.Lock()
	g.balances[subAccountID] = balance
	g.mu.Unlock()
}

// Deduct subtracts cost from subAccountID's balance; a subAccountID that
// doesn't exist yet starts from defaultBalance.
//
// Returns the balance remaining after the deduction; a negative value means
// this request pushed the balance below zero (called after M10 completes,
// deducting after the fact — the current request is not rejected; it only
// takes effect on the next Check).
//
// Not called automatically until outbox event subscription is wired up —
// left as a public API in v0.5 so external scripts / tests can simulate it.
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

// GetBalance reads the current balance (for ops / tests).
func (g *InMemoryBudgetGate) GetBalance(subAccountID string) float64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	balance, ok := g.balances[subAccountID]
	if !ok {
		return g.defaultBalance
	}
	return balance
}

// Compile-time assertion: InMemoryBudgetGate satisfies BudgetGate.
var _ BudgetGate = (*InMemoryBudgetGate)(nil)
