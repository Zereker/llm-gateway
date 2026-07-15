package domain

import (
	"math"
	"testing"
)

func TestModelRoutingDecisionRepresentsConcreteFallback(t *testing.T) {
	decision := ModelRoutingDecision{
		RequestedModel: "gpt-4o",
		Outcome:        RoutingOutcomeResolved,
		Reason:         RoutingReasonConcreteModel,
		Candidates: []RoutingCandidateDecision{
			{ModelServiceID: 1, Model: "gpt-4o", Source: RoutingCandidateRequested, Eligible: true, Reason: RoutingReasonConcreteModel, Order: 0},
			{ModelServiceID: 2, Model: "claude-sonnet", Source: RoutingCandidateFallbackHeader, Eligible: true, Reason: RoutingReasonFallbackAccepted, Order: 1},
			{Model: "missing", Source: RoutingCandidateFallbackHeader, Eligible: false, Reason: RoutingReasonCandidateNotFound, Order: 2},
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

func TestModelRoutingDecisionRejectsMalformedResolverOutput(t *testing.T) {
	valid := ModelRoutingDecision{
		RequestedModel: "fast-chat",
		VirtualModel:   true,
		Outcome:        RoutingOutcomeResolved,
		Reason:         RoutingReasonVirtualPolicyMatched,
		Policy:         &RoutingPolicyRef{ID: "route-fast-chat", Version: 1, Scope: RoutingScope{Kind: RoutingScopeGlobal}},
		Candidates: []RoutingCandidateDecision{
			{Model: "gpt-4o-mini", Source: RoutingCandidatePolicy, Eligible: true, Reason: RoutingReasonVirtualPolicyMatched},
		},
	}

	tests := map[string]func(*ModelRoutingDecision){
		"missing requested model": func(d *ModelRoutingDecision) { d.RequestedModel = "" },
		"missing outcome":         func(d *ModelRoutingDecision) { d.Outcome = "" },
		"missing reason":          func(d *ModelRoutingDecision) { d.Reason = "" },
		"missing policy ID":       func(d *ModelRoutingDecision) { d.Policy.ID = "" },
		"missing policy version":  func(d *ModelRoutingDecision) { d.Policy.Version = 0 },
		"missing candidate model": func(d *ModelRoutingDecision) { d.Candidates[0].Model = "" },
		"missing candidate source": func(d *ModelRoutingDecision) {
			d.Candidates[0].Source = ""
		},
		"missing candidate reason": func(d *ModelRoutingDecision) {
			d.Candidates[0].Reason = ""
		},
		"resolved without eligible candidate": func(d *ModelRoutingDecision) {
			d.Candidates[0].Eligible = false
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			decision := valid
			policy := *valid.Policy
			decision.Policy = &policy
			decision.Candidates = append([]RoutingCandidateDecision(nil), valid.Candidates...)
			mutate(&decision)
			if err := decision.Validate(); err == nil {
				t.Fatal("Validate() succeeded for malformed decision")
			}
		})
	}
}

func TestModelRoutingDecisionValidatesScoreExplanation(t *testing.T) {
	newDecision := func() ModelRoutingDecision {
		return ModelRoutingDecision{
			RequestedModel: "fast-chat", VirtualModel: true,
			Outcome: RoutingOutcomeResolved, Reason: RoutingReasonVirtualPolicyMatched,
			Policy: &RoutingPolicyRef{ID: "rp_fast", Version: 2},
			Candidates: []RoutingCandidateDecision{{
				Model: "small", Source: RoutingCandidatePolicy, Eligible: true,
				Reason: RoutingReasonVirtualPolicyMatched,
				Score: &RoutingScoreExplanation{
					LatencySource: RoutingSignalObserved, CostSource: RoutingSignalConfigured,
					LatencyScore: 0.8, CostScore: 0.7, TotalScore: 0.75,
					CostProfile: &RoutingCostProfileRef{ID: "rcp_small", Version: 3},
				},
			}},
		}
	}

	if err := newDecision().Validate(); err != nil {
		t.Fatalf("valid score rejected: %v", err)
	}

	tests := map[string]func(*RoutingScoreExplanation){
		"missing latency source": func(s *RoutingScoreExplanation) { s.LatencySource = "" },
		"missing cost source":    func(s *RoutingScoreExplanation) { s.CostSource = "" },
		"negative latency":       func(s *RoutingScoreExplanation) { s.LatencyScore = -0.1 },
		"cost above one":         func(s *RoutingScoreExplanation) { s.CostScore = 1.1 },
		"total NaN":              func(s *RoutingScoreExplanation) { s.TotalScore = math.NaN() },
		"total infinity":         func(s *RoutingScoreExplanation) { s.TotalScore = math.Inf(1) },
		"missing profile ID":     func(s *RoutingScoreExplanation) { s.CostProfile.ID = "" },
		"missing profile version": func(s *RoutingScoreExplanation) {
			s.CostProfile.Version = 0
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			decision := newDecision()
			mutate(decision.Candidates[0].Score)
			if err := decision.Validate(); err == nil {
				t.Fatal("Validate() accepted an invalid score")
			}
		})
	}
}
