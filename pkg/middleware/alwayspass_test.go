package middleware

import (
	"context"
	"testing"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

func TestAlwaysPassGate_AlwaysActive(t *testing.T) {
	g := AlwaysPassGate{}
	for _, uid := range []string{"alice", "", "bob"} {
		st, err := g.Check(context.Background(), uid)
		if err != nil {
			t.Fatalf("Check(%q): err %v", uid, err)
		}
		if st != domain.BudgetActive {
			t.Errorf("Check(%q) = %v, want BudgetActive", uid, st)
		}
	}
}
