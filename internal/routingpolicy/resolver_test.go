package routingpolicy

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/zereker/llm-gateway/internal/domain"
)

type stubPolicies struct {
	policy *domain.RoutingPolicy
	err    error
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
