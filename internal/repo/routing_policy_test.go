package repo

import (
	"context"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/internal/domain"
)

func TestSQLRoutingPolicyReaderUsesMostSpecificScope(t *testing.T) {
	db := newTestDB(t)
	for _, row := range []struct {
		id, scope, scopeID, candidate string
	}{
		{"rp_global", "global", "", "global-model"},
		{"rp_account", "account", "default", "account-model"},
	} {
		if _, err := db.Exec(
			`INSERT INTO routing_policies
			 (policy_id, version, scope_kind, scope_id, virtual_model, rule_json)
			 VALUES (?, 1, ?, ?, 'fast-chat', JSON_OBJECT('candidates', JSON_ARRAY(JSON_OBJECT('model', ?))))`,
			row.id, row.scope, row.scopeID, row.candidate); err != nil {
			t.Fatal(err)
		}
	}

	reader := NewSQLRoutingPolicyReader(db)
	policy, err := reader.GetEffective(context.Background(), "default", "", "fast-chat")
	if err != nil {
		t.Fatal(err)
	}
	if policy.Ref.ID != "rp_account" || policy.Candidates[0].Model != "account-model" {
		t.Fatalf("policy=%+v", policy)
	}

	missing, err := reader.GetEffective(context.Background(), "default", "", "missing")
	if err != nil || missing != nil {
		t.Fatalf("missing policy=%+v err=%v", missing, err)
	}
}

type countingPolicyReader struct {
	policy *domain.RoutingPolicy
	calls  int
}

func (r *countingPolicyReader) GetEffective(context.Context, string, string, string) (*domain.RoutingPolicy, error) {
	r.calls++
	return r.policy, nil
}

func TestCachedRoutingPolicyReaderCachesPositiveNegativeAndEvicts(t *testing.T) {
	inner := &countingPolicyReader{}
	reader := NewCachedRoutingPolicyReader(inner, 8, time.Minute, nil)
	ctx := context.Background()

	for range 2 {
		if policy, err := reader.GetEffective(ctx, "a", "", "fast"); err != nil || policy != nil {
			t.Fatalf("policy=%+v err=%v", policy, err)
		}
	}
	if inner.calls != 1 {
		t.Fatalf("negative cache calls=%d", inner.calls)
	}

	reader.EvictAll()
	inner.policy = &domain.RoutingPolicy{VirtualModel: "fast"}
	for range 2 {
		if policy, err := reader.GetEffective(ctx, "a", "", "fast"); err != nil || policy == nil {
			t.Fatalf("policy=%+v err=%v", policy, err)
		}
	}
	if inner.calls != 2 {
		t.Fatalf("positive cache calls=%d", inner.calls)
	}
}
