package routingpolicy

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/internal/domain"
)

type stubPolicies struct {
	policy *domain.RoutingPolicy
	err    error
}

type stubCosts struct {
	profiles map[int64]*domain.RoutingCostProfile
	err      error
	calls    int
}

func (s *stubCosts) GetActive(_ context.Context, id int64) (*domain.RoutingCostProfile, error) {
	s.calls++
	return s.profiles[id], s.err
}

type stubTelemetry struct {
	models map[string][]EndpointTelemetry
	calls  int
}

func (s *stubTelemetry) ForModel(_ context.Context, model, _ string) ([]EndpointTelemetry, error) {
	s.calls++
	return s.models[model], nil
}

func (s stubPolicies) GetEffective(context.Context, string, string, string) (*domain.RoutingPolicy, error) {
	return s.policy, s.err
}

type stubCatalog struct {
	models map[string]*domain.ModelService
	err    error
}

func (s stubCatalog) GetByModel(_ context.Context, model string) (*domain.ModelService, error) {
	return s.models[model], s.err
}

type stubSubscriptions struct {
	allowed map[int64]bool
	err     error
}

func (s stubSubscriptions) HasModel(_ context.Context, _ string, id int64) (bool, error) {
	return s.allowed[id], s.err
}

func basePolicy() *domain.RoutingPolicy {
	return &domain.RoutingPolicy{
		Ref:          domain.RoutingPolicyRef{ID: "rp_fast", Version: 3, Scope: domain.RoutingScope{Kind: domain.RoutingScopeAccount, ID: "a1"}},
		VirtualModel: "fast-chat",
		MaxAttempts:  2,
		Candidates: []domain.RoutingPolicyCandidate{
			{Model: "small", Weight: 10, Modalities: []domain.Modality{domain.ModalityChat}},
			{Model: "large", Weight: 100, Regions: []string{"cn-north"}},
			{Model: "denied", Weight: 1000},
		},
		Constraints: domain.RoutingConstraints{DenyModels: []string{"denied"}},
	}
}

func TestResolveOrdersEligibleCandidatesAndExplainsRejections(t *testing.T) {
	policy := basePolicy()
	resolver := NewResolver(
		stubPolicies{policy: policy},
		stubCatalog{models: map[string]*domain.ModelService{
			"small": {ID: 1, Model: "small"},
			"large": {ID: 2, Model: "large"},
		}},
		stubSubscriptions{allowed: map[int64]bool{1: true, 2: true}},
	)

	resolution, err := resolver.Resolve(context.Background(), Input{
		RequestedModel: "fast-chat", AccountID: "a1", Region: "CN-NORTH", Modality: domain.ModalityChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resolution.Decision.EligibleModels(), []string{"large", "small"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("eligible models = %v, want %v", got, want)
	}
	if got := resolution.Decision.Candidates[2].Reason; got != domain.RoutingReasonCandidateDenied {
		t.Fatalf("denied reason = %q", got)
	}
	if resolution.Decision.MaxAttempts != 2 || resolution.Decision.Policy.Version != 3 {
		t.Fatalf("decision lost policy metadata: %+v", resolution.Decision)
	}
}

func TestResolveFiltersCatalogSubscriptionAndConstraints(t *testing.T) {
	policy := basePolicy()
	policy.Constraints.AllowModels = []string{"wrong-region", "wrong-modality", "missing", "unsubscribed"}
	policy.Candidates = []domain.RoutingPolicyCandidate{
		{Model: "wrong-region", Regions: []string{"eu"}},
		{Model: "wrong-modality", Modalities: []domain.Modality{domain.ModalityImage}},
		{Model: "not-allowed"},
		{Model: "missing"},
		{Model: "unsubscribed"},
	}
	resolver := NewResolver(stubPolicies{policy: policy}, stubCatalog{models: map[string]*domain.ModelService{
		"unsubscribed": {ID: 9, Model: "unsubscribed"},
	}}, stubSubscriptions{allowed: map[int64]bool{}})

	resolution, err := resolver.Resolve(context.Background(), Input{
		RequestedModel: "fast-chat", AccountID: "a1", Region: "cn", Modality: domain.ModalityChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Decision.Outcome != domain.RoutingOutcomeRejected || resolution.Decision.Reason != domain.RoutingReasonNoEligibleCandidate {
		t.Fatalf("decision = %+v", resolution.Decision)
	}
	wantReasons := map[domain.RoutingReasonCode]bool{
		domain.RoutingReasonCandidateRegionMismatch:   true,
		domain.RoutingReasonCandidateModalityMismatch: true,
		domain.RoutingReasonCandidateNotAllowed:       true,
		domain.RoutingReasonCandidateNotFound:         true,
		domain.RoutingReasonCandidateNotSubscribed:    true,
	}
	for _, candidate := range resolution.Decision.Candidates {
		delete(wantReasons, candidate.Reason)
	}
	if len(wantReasons) != 0 {
		t.Fatalf("missing reasons: %v; decision=%+v", wantReasons, resolution.Decision)
	}
}

func TestResolveFailureSemantics(t *testing.T) {
	t.Run("no policy", func(t *testing.T) {
		resolution, err := NewResolver(stubPolicies{}, stubCatalog{}, stubSubscriptions{}).
			Resolve(context.Background(), Input{RequestedModel: "missing"})
		if err != nil || resolution.Decision.Reason != domain.RoutingReasonNoPolicy {
			t.Fatalf("resolution=%+v err=%v", resolution, err)
		}
	})

	t.Run("policy dependency", func(t *testing.T) {
		resolution, err := NewResolver(stubPolicies{err: errors.New("db down")}, stubCatalog{}, stubSubscriptions{}).
			Resolve(context.Background(), Input{RequestedModel: "fast-chat"})
		if err == nil || resolution.Decision.Reason != domain.RoutingReasonPolicyUnavailable {
			t.Fatalf("resolution=%+v err=%v", resolution, err)
		}
	})

	t.Run("invalid policy", func(t *testing.T) {
		policy := basePolicy()
		policy.Candidates = nil
		resolution, err := NewResolver(stubPolicies{policy: policy}, stubCatalog{}, stubSubscriptions{}).
			Resolve(context.Background(), Input{RequestedModel: "fast-chat"})
		if err == nil || resolution.Decision.Reason != domain.RoutingReasonPolicyInvalid {
			t.Fatalf("resolution=%+v err=%v", resolution, err)
		}
	})

	t.Run("catalog dependency", func(t *testing.T) {
		resolution, err := NewResolver(stubPolicies{policy: basePolicy()}, stubCatalog{err: errors.New("db down")}, stubSubscriptions{}).
			Resolve(context.Background(), Input{RequestedModel: "fast-chat", Modality: domain.ModalityChat, Region: "cn-north"})
		if err == nil || resolution.Decision.Policy == nil {
			t.Fatalf("resolution=%+v err=%v", resolution, err)
		}
	})

	t.Run("subscription dependency", func(t *testing.T) {
		resolution, err := NewResolver(
			stubPolicies{policy: basePolicy()},
			stubCatalog{models: map[string]*domain.ModelService{"small": {ID: 1, Model: "small"}}},
			stubSubscriptions{err: errors.New("db down")},
		).Resolve(context.Background(), Input{RequestedModel: "fast-chat", Modality: domain.ModalityChat})
		if err == nil || resolution.Decision.Policy == nil {
			t.Fatalf("resolution=%+v err=%v", resolution, err)
		}
	})
}

func TestValidatePolicyRejectsMalformedSnapshots(t *testing.T) {
	tests := map[string]func(*domain.RoutingPolicy){
		"identity mismatch": func(p *domain.RoutingPolicy) { p.VirtualModel = "other" },
		"missing policy ID": func(p *domain.RoutingPolicy) { p.Ref.ID = "" },
		"missing version":   func(p *domain.RoutingPolicy) { p.Ref.Version = 0 },
		"missing candidates": func(p *domain.RoutingPolicy) {
			p.Candidates = nil
		},
		"negative attempts": func(p *domain.RoutingPolicy) { p.MaxAttempts = -1 },
		"empty candidate":   func(p *domain.RoutingPolicy) { p.Candidates[0].Model = "" },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			policy := basePolicy()
			policy.Candidates = append([]domain.RoutingPolicyCandidate(nil), policy.Candidates...)
			mutate(policy)
			if err := validatePolicy(policy, "fast-chat"); err == nil {
				t.Fatal("validatePolicy succeeded")
			}
		})
	}
}

func TestResolveScoresLatencyAndCostWithExplainableInputs(t *testing.T) {
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	policy := basePolicy()
	policy.Candidates = []domain.RoutingPolicyCandidate{{Model: "fast", Weight: 1}, {Model: "cheap", Weight: 100}}
	policy.Objectives = domain.RoutingObjectives{
		LatencyWeight: 2, CostWeight: 1, TargetLatencyMs: 100, TargetCostMicrousd: 100,
		EstimatedInputTokens: 1_000_000, MinTelemetrySamples: 1, TelemetryMaxAgeSeconds: 60,
	}
	costs := &stubCosts{profiles: map[int64]*domain.RoutingCostProfile{
		1: {Ref: domain.RoutingCostProfileRef{ID: "fast-cost", Version: 2}, ModelServiceID: 1, InputMicrousdPerMillionToken: 200},
		2: {Ref: domain.RoutingCostProfileRef{ID: "cheap-cost", Version: 4}, ModelServiceID: 2, InputMicrousdPerMillionToken: 50},
	}}
	telemetry := &stubTelemetry{models: map[string][]EndpointTelemetry{
		"fast":  {{LatencyMs: 50, SuccessRate: 1, SampleCount: 10, Updated: now}},
		"cheap": {{LatencyMs: 200, SuccessRate: 1, SampleCount: 10, Updated: now}},
	}}
	resolver := NewResolver(stubPolicies{policy: policy}, stubCatalog{models: map[string]*domain.ModelService{
		"fast": {ID: 1, Model: "fast"}, "cheap": {ID: 2, Model: "cheap"},
	}}, stubSubscriptions{allowed: map[int64]bool{1: true, 2: true}}, WithObjectives(costs, telemetry))
	resolver.now = func() time.Time { return now }

	resolution, err := resolver.Resolve(context.Background(), Input{RequestedModel: "fast-chat", AccountID: "a1", Modality: domain.ModalityChat})
	if err != nil {
		t.Fatal(err)
	}
	if got := resolution.Decision.EligibleModels(); !reflect.DeepEqual(got, []string{"fast", "cheap"}) {
		t.Fatalf("objective order = %v", got)
	}
	first := resolution.Decision.Candidates[0].Score
	if first == nil || first.LatencySource != domain.RoutingSignalObserved || first.CostProfile.Version != 2 || first.EstimatedMicrousd != 200 {
		t.Fatalf("score explanation = %+v", first)
	}
	if first.TotalScore <= resolution.Decision.Candidates[1].Score.TotalScore {
		t.Fatalf("scores did not prefer latency objective: %+v", resolution.Decision.Candidates)
	}
}

func TestResolveUsesExplicitNeutralForMissingAndStaleSignals(t *testing.T) {
	now := time.Now().UTC()
	policy := basePolicy()
	policy.Candidates = []domain.RoutingPolicyCandidate{{Model: "missing"}, {Model: "stale"}}
	policy.Objectives = domain.RoutingObjectives{LatencyWeight: 1, TargetLatencyMs: 100, MinTelemetrySamples: 1, TelemetryMaxAgeSeconds: 10}
	telemetry := &stubTelemetry{models: map[string][]EndpointTelemetry{
		"stale": {{LatencyMs: 10, SuccessRate: 1, SampleCount: 5, Updated: now.Add(-time.Minute)}},
	}}
	resolver := NewResolver(stubPolicies{policy: policy}, stubCatalog{models: map[string]*domain.ModelService{
		"missing": {ID: 1, Model: "missing"}, "stale": {ID: 2, Model: "stale"},
	}}, stubSubscriptions{allowed: map[int64]bool{1: true, 2: true}}, WithObjectives(nil, telemetry))
	resolver.now = func() time.Time { return now }
	resolution, err := resolver.Resolve(context.Background(), Input{RequestedModel: "fast-chat", Modality: domain.ModalityChat})
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]domain.RoutingSignalSource{}
	for _, candidate := range resolution.Decision.Candidates {
		sources[candidate.Model] = candidate.Score.LatencySource
	}
	if sources["missing"] != domain.RoutingSignalMissing || sources["stale"] != domain.RoutingSignalStale {
		t.Fatalf("sources = %v", sources)
	}
}

func TestResolveExplorationIsDeterministicAndNeverBypassesHardFilters(t *testing.T) {
	policy := basePolicy()
	policy.Candidates = []domain.RoutingPolicyCandidate{{Model: "primary", Weight: 100}, {Model: "explore", Weight: 1}, {Model: "denied", Weight: 1000}}
	policy.Constraints = domain.RoutingConstraints{AllowModels: []string{"primary", "explore", "denied"}, DenyModels: []string{"denied"}}
	policy.Objectives = domain.RoutingObjectives{LatencyWeight: 1, TargetLatencyMs: 100, ExplorationPermille: 1000}
	resolver := NewResolver(stubPolicies{policy: policy}, stubCatalog{models: map[string]*domain.ModelService{
		"primary": {ID: 1, Model: "primary"}, "explore": {ID: 2, Model: "explore"},
	}}, stubSubscriptions{allowed: map[int64]bool{1: true, 2: true}})
	in := Input{RequestedModel: "fast-chat", Modality: domain.ModalityChat, DecisionKey: "req-fixed"}
	first, err := resolver.Resolve(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolver.Resolve(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Decision.EligibleModels(), second.Decision.EligibleModels()) || first.Decision.EligibleModels()[0] != "explore" {
		t.Fatalf("exploration is not deterministic: %v / %v", first.Decision.EligibleModels(), second.Decision.EligibleModels())
	}
	if first.Decision.Candidates[0].Score == nil || !first.Decision.Candidates[0].Score.ExplorationChosen {
		t.Fatalf("exploration not explained: %+v", first.Decision.Candidates[0])
	}
	for _, candidate := range first.Decision.Candidates {
		if candidate.Model == "denied" && candidate.Eligible {
			t.Fatal("exploration bypassed deny constraint")
		}
	}
}

func TestResolveAllIneligibleSkipsObjectiveDependencies(t *testing.T) {
	policy := basePolicy()
	policy.Constraints = domain.RoutingConstraints{DenyModels: []string{"small", "large", "denied"}}
	policy.Objectives = domain.RoutingObjectives{LatencyWeight: 1, CostWeight: 1, TargetLatencyMs: 100,
		TargetCostMicrousd: 10, EstimatedInputTokens: 1}
	costs, telemetry := &stubCosts{}, &stubTelemetry{}
	resolution, err := NewResolver(stubPolicies{policy: policy}, stubCatalog{}, stubSubscriptions{}, WithObjectives(costs, telemetry)).
		Resolve(context.Background(), Input{RequestedModel: "fast-chat", Modality: domain.ModalityChat})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Decision.Reason != domain.RoutingReasonNoEligibleCandidate || costs.calls != 0 || telemetry.calls != 0 {
		t.Fatalf("resolution=%+v cost_calls=%d telemetry_calls=%d", resolution, costs.calls, telemetry.calls)
	}
}
