package domain

import "testing"

func TestModelRoutingDecisionRepresentsExistingConcreteFallback(t *testing.T) {
	decision := ModelRoutingDecision{
		RequestedModel: "gpt-4o",
		Outcome:        RoutingOutcomeResolved,
		Reason:         RoutingReasonConcreteModel,
		Candidates: []RoutingCandidateDecision{
			{ModelServiceID: 1, Model: "gpt-4o", Source: RoutingCandidateRequested, Eligible: true, Reason: RoutingReasonConcreteModel, Order: 0},
			{ModelServiceID: 2, Model: "claude-sonnet", Source: RoutingCandidateLegacyHeader, Eligible: true, Reason: RoutingReasonLegacyFallbackAccepted, Order: 1},
			{Model: "missing", Source: RoutingCandidateLegacyHeader, Eligible: false, Reason: RoutingReasonCandidateNotFound, Order: 2},
		},
		MaxAttempts: 3,
	}

	if err := decision.Validate(); err != nil {
		t.Fatal(err)
	}
	want := []string{"gpt-4o", "claude-sonnet"}
	got := decision.EligibleModels()
	if len(got) != len(want) {
		t.Fatalf("eligible models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("eligible models = %v, want %v", got, want)
		}
	}
}

func TestModelRoutingDecisionRequiresPolicyForVirtualModel(t *testing.T) {
	decision := ModelRoutingDecision{
		RequestedModel: "fast-chat",
		VirtualModel:   true,
		Outcome:        RoutingOutcomeResolved,
		Reason:         RoutingReasonVirtualPolicyMatched,
		Candidates: []RoutingCandidateDecision{
			{Model: "gpt-4o-mini", Source: RoutingCandidatePolicy, Eligible: true, Reason: RoutingReasonVirtualPolicyMatched},
		},
	}

	if err := decision.Validate(); err == nil {
		t.Fatal("Validate() succeeded without a policy reference")
	}
	decision.Policy = &RoutingPolicyRef{ID: "route-fast-chat", Version: 7, Scope: RoutingScope{Kind: RoutingScopeAccount, ID: "default"}}
	if err := decision.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRejectedRoutingDecisionMayHaveNoEligibleCandidates(t *testing.T) {
	decision := ModelRoutingDecision{
		RequestedModel: "fast-chat",
		VirtualModel:   true,
		Outcome:        RoutingOutcomeRejected,
		Reason:         RoutingReasonNoEligibleCandidate,
		Policy:         &RoutingPolicyRef{ID: "route-fast-chat", Version: 2, Scope: RoutingScope{Kind: RoutingScopeGlobal}},
		Candidates: []RoutingCandidateDecision{
			{Model: "gpt-4o", Source: RoutingCandidatePolicy, Reason: RoutingReasonCandidateNotSubscribed},
		},
	}

	if err := decision.Validate(); err != nil {
		t.Fatal(err)
	}
}
