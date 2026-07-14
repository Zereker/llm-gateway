// Package routingpolicy resolves caller-facing virtual models into validated
// concrete model chains. It never selects endpoints or invokes upstreams.
package routingpolicy

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/zereker/llm-gateway/internal/domain"
)

type PolicyReader interface {
	GetEffective(ctx context.Context, accountID, projectID, virtualModel string) (*domain.RoutingPolicy, error)
}

type ModelCatalog interface {
	GetByModel(ctx context.Context, model string) (*domain.ModelService, error)
}

type SubscriptionChecker interface {
	HasModel(ctx context.Context, accountID string, modelServiceID int64) (bool, error)
}

type Input struct {
	RequestedModel string
	AccountID      string
	ProjectID      string
	Region         string
	Modality       domain.Modality
}

type Resolution struct {
	Decision domain.ModelRoutingDecision
	Chain    []*domain.ModelService
}

type Resolver struct {
	policies      PolicyReader
	catalog       ModelCatalog
	subscriptions SubscriptionChecker
}

func NewResolver(policies PolicyReader, catalog ModelCatalog, subscriptions SubscriptionChecker) *Resolver {
	return &Resolver{policies: policies, catalog: catalog, subscriptions: subscriptions}
}

func (r *Resolver) Resolve(ctx context.Context, in Input) (Resolution, error) {
	policy, err := r.policies.GetEffective(ctx, in.AccountID, in.ProjectID, in.RequestedModel)
	if err != nil {
		return failed(in.RequestedModel, domain.RoutingReasonPolicyUnavailable), fmt.Errorf("resolve routing policy: %w", err)
	}

	if policy == nil {
		return rejected(in.RequestedModel, domain.RoutingReasonNoPolicy), nil
	}

	if err := validatePolicy(policy, in.RequestedModel); err != nil {
		return failed(in.RequestedModel, domain.RoutingReasonPolicyInvalid), err
	}

	decision := domain.ModelRoutingDecision{
		RequestedModel: in.RequestedModel,
		VirtualModel:   true,
		Outcome:        domain.RoutingOutcomeResolved,
		Reason:         domain.RoutingReasonVirtualPolicyMatched,
		Policy:         &policy.Ref,
		MaxAttempts:    policy.MaxAttempts,
		Candidates:     make([]domain.RoutingCandidateDecision, 0, len(policy.Candidates)),
	}
	resolved := make(map[string]*domain.ModelService, len(policy.Candidates))

	for configuredOrder, candidate := range policy.Candidates {
		evaluation := domain.RoutingCandidateDecision{
			Model:  candidate.Model,
			Source: domain.RoutingCandidatePolicy,
			Reason: domain.RoutingReasonVirtualPolicyMatched,
			Order:  configuredOrder,
			Weight: candidate.Weight,
		}

		if reason := constraintReason(in, policy.Constraints, candidate); reason != "" {
			evaluation.Reason = reason
			decision.Candidates = append(decision.Candidates, evaluation)

			continue
		}

		model, lookupErr := r.catalog.GetByModel(ctx, candidate.Model)
		if lookupErr != nil {
			return failedWithPolicy(in.RequestedModel, policy, domain.RoutingReasonPolicyUnavailable),
				fmt.Errorf("resolve candidate %q: %w", candidate.Model, lookupErr)
		}

		if model == nil {
			evaluation.Reason = domain.RoutingReasonCandidateNotFound
			decision.Candidates = append(decision.Candidates, evaluation)

			continue
		}

		subscribed, subscriptionErr := r.subscriptions.HasModel(ctx, in.AccountID, model.ID)
		if subscriptionErr != nil {
			return failedWithPolicy(in.RequestedModel, policy, domain.RoutingReasonPolicyUnavailable),
				fmt.Errorf("resolve candidate %q subscription: %w", candidate.Model, subscriptionErr)
		}

		if !subscribed {
			evaluation.ModelServiceID = model.ID
			evaluation.Reason = domain.RoutingReasonCandidateNotSubscribed
			decision.Candidates = append(decision.Candidates, evaluation)

			continue
		}

		evaluation.ModelServiceID = model.ID
		evaluation.Eligible = true
		decision.Candidates = append(decision.Candidates, evaluation)
		resolved[candidate.Model] = model
	}

	// Higher weights are preferred; equal weights retain configured order.
	sort.SliceStable(decision.Candidates, func(i, j int) bool {
		left, right := decision.Candidates[i], decision.Candidates[j]
		if left.Eligible != right.Eligible {
			return left.Eligible
		}

		if left.Eligible && left.Weight != right.Weight {
			return left.Weight > right.Weight
		}

		return left.Order < right.Order
	})

	chain := make([]*domain.ModelService, 0, len(resolved))
	for order := range decision.Candidates {
		decision.Candidates[order].Order = order
		if decision.Candidates[order].Eligible {
			chain = append(chain, resolved[decision.Candidates[order].Model])
		}
	}

	if len(chain) == 0 {
		decision.Outcome = domain.RoutingOutcomeRejected
		decision.Reason = domain.RoutingReasonNoEligibleCandidate
	}

	return Resolution{Decision: decision, Chain: chain}, nil
}

func validatePolicy(policy *domain.RoutingPolicy, requested string) error {
	if policy.VirtualModel != requested || policy.Ref.ID == "" || policy.Ref.Version == 0 {
		return fmt.Errorf("routing policy: identity is incomplete or mismatched")
	}

	if len(policy.Candidates) == 0 {
		return fmt.Errorf("routing policy: candidates are required")
	}

	if policy.MaxAttempts < 0 {
		return fmt.Errorf("routing policy: max_attempts cannot be negative")
	}

	for i, candidate := range policy.Candidates {
		if candidate.Model == "" {
			return fmt.Errorf("routing policy: candidate %d model is required", i)
		}
	}

	return nil
}

func constraintReason(
	in Input,
	constraints domain.RoutingConstraints,
	candidate domain.RoutingPolicyCandidate,
) domain.RoutingReasonCode {
	if contains(constraints.DenyModels, candidate.Model) {
		return domain.RoutingReasonCandidateDenied
	}

	if len(constraints.AllowModels) > 0 && !contains(constraints.AllowModels, candidate.Model) {
		return domain.RoutingReasonCandidateNotAllowed
	}

	if !regionAllowed(in.Region, constraints.Regions) || !regionAllowed(in.Region, candidate.Regions) {
		return domain.RoutingReasonCandidateRegionMismatch
	}

	if !modalityAllowed(in.Modality, constraints.Modalities) || !modalityAllowed(in.Modality, candidate.Modalities) {
		return domain.RoutingReasonCandidateModalityMismatch
	}

	return ""
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}

	return false
}

func regionAllowed(region string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}

	for _, value := range allowed {
		if strings.EqualFold(value, region) {
			return true
		}
	}

	return false
}

func modalityAllowed(modality domain.Modality, allowed []domain.Modality) bool {
	if len(allowed) == 0 {
		return true
	}

	for _, value := range allowed {
		if value == modality {
			return true
		}
	}

	return false
}

func rejected(model string, reason domain.RoutingReasonCode) Resolution {
	return Resolution{Decision: domain.ModelRoutingDecision{
		RequestedModel: model,
		VirtualModel:   true,
		Outcome:        domain.RoutingOutcomeRejected,
		Reason:         reason,
	}}
}

func failed(model string, reason domain.RoutingReasonCode) Resolution {
	return Resolution{Decision: domain.ModelRoutingDecision{
		RequestedModel: model,
		VirtualModel:   true,
		Outcome:        domain.RoutingOutcomeDependencyFailure,
		Reason:         reason,
	}}
}

func failedWithPolicy(
	model string,
	policy *domain.RoutingPolicy,
	reason domain.RoutingReasonCode,
) Resolution {
	resolution := failed(model, reason)
	resolution.Decision.Policy = &policy.Ref

	return resolution
}
