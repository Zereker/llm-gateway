package middleware

import (
	"context"

	"github.com/zereker/llm-gateway/internal/domain"
)

// AlwaysPassGate is the zero-dependency default implementation of BudgetGate:
// it always returns BudgetActive.
//
// Used for scenarios without billing / quota systems (development, single-tenant
// internal gateways). Implement a custom BudgetGate when integrating with an
// external billing system.
//
// Zero value is ready to use: var gate AlwaysPassGate; no constructor needed.
type AlwaysPassGate struct{}

// Check implements BudgetGate.Check: always BudgetActive, always nil.
func (AlwaysPassGate) Check(_ context.Context, _ string) (domain.BudgetStatus, error) {
	return domain.BudgetActive, nil
}

// Compile-time assertion: AlwaysPassGate satisfies BudgetGate.
var _ BudgetGate = AlwaysPassGate{}
