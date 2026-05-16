package middleware

import (
	"context"
	"sync"
	"testing"

	"github.com/zereker/llm-gateway/pkg/domain"
)

func TestInMemoryBudget_DefaultZero_Inactive(t *testing.T) {
	g := NewInMemoryBudgetGate(0)
	got, err := g.Check(context.Background(), "u1")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != domain.BudgetInactive {
		t.Errorf("status=%v, want=inactive", got)
	}
}

func TestInMemoryBudget_DefaultPositive_Active(t *testing.T) {
	g := NewInMemoryBudgetGate(10)
	got, _ := g.Check(context.Background(), "u1")
	if got != domain.BudgetActive {
		t.Errorf("status=%v, want=active", got)
	}
}

func TestInMemoryBudget_SetBalance_OverridesDefault(t *testing.T) {
	g := NewInMemoryBudgetGate(10) // default active
	g.SetBalance("u1", 0)
	got, _ := g.Check(context.Background(), "u1")
	if got != domain.BudgetInactive {
		t.Errorf("status=%v, want=inactive after SetBalance(0)", got)
	}
	if bal := g.GetBalance("u1"); bal != 0 {
		t.Errorf("balance=%v, want=0", bal)
	}
}

func TestInMemoryBudget_GetBalance_FallsBackToDefault(t *testing.T) {
	g := NewInMemoryBudgetGate(7.5)
	if bal := g.GetBalance("u_never_set"); bal != 7.5 {
		t.Errorf("balance=%v, want=7.5", bal)
	}
}

func TestInMemoryBudget_Deduct_ReducesBalance(t *testing.T) {
	g := NewInMemoryBudgetGate(10)
	got := g.Deduct("u1", 3)
	if got != 7 {
		t.Errorf("Deduct returned %v, want=7", got)
	}
	if bal := g.GetBalance("u1"); bal != 7 {
		t.Errorf("post-deduct balance=%v, want=7", bal)
	}
}

func TestInMemoryBudget_Deduct_GoesNegative_NoFloor(t *testing.T) {
	g := NewInMemoryBudgetGate(5)
	got := g.Deduct("u1", 8)
	if got != -3 {
		t.Errorf("balance=%v, want=-3 (no floor)", got)
	}
	st, _ := g.Check(context.Background(), "u1")
	if st != domain.BudgetInactive {
		t.Errorf("after-overdraft status=%v, want=inactive", st)
	}
}

func TestInMemoryBudget_Deduct_FromDefault(t *testing.T) {
	g := NewInMemoryBudgetGate(10)
	// 没显式 SetBalance；Deduct 从 default 起算
	got := g.Deduct("u_never_set", 4)
	if got != 6 {
		t.Errorf("deduct from default: got=%v, want=6", got)
	}
}

func TestInMemoryBudget_Concurrent_Safety(t *testing.T) {
	g := NewInMemoryBudgetGate(1000)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = g.Check(context.Background(), "u_race") }()
		go func() { defer wg.Done(); g.Deduct("u_race", 1) }()
	}
	wg.Wait()
	// 50 次 deduct，每次 1 → 余额 = 1000 - 50 = 950
	if bal := g.GetBalance("u_race"); bal != 950 {
		t.Errorf("balance=%v, want=950", bal)
	}
}
