// Package routingpolicy resolves caller-facing virtual models into validated
// concrete model chains. It never selects endpoints or invokes upstreams.
package routingpolicy

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

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

type CostReader interface {
	GetActive(ctx context.Context, modelServiceID int64) (*domain.RoutingCostProfile, error)
}

type EndpointTelemetry struct {
	LatencyMs   float64   `json:"latency_ms"`
	SuccessRate float64   `json:"success_rate"`
	SampleCount uint32    `json:"sample_count"`
	Updated     time.Time `json:"updated_at"`
}

type TelemetryReader interface {
	ForModel(ctx context.Context, model, group string) ([]EndpointTelemetry, error)
}

type Input struct {
	RequestedModel string
	AccountID      string
	ProjectID      string
	Region         string
	Modality       domain.Modality
	Group          string
	DecisionKey    string
}

type Resolution struct {
	Decision domain.ModelRoutingDecision
	Chain    []*domain.ModelService
}

type Resolver struct {
	policies      PolicyReader
	catalog       ModelCatalog
	subscriptions SubscriptionChecker
	costs         CostReader
	telemetry     TelemetryReader
	now           func() time.Time
}

type Option func(*Resolver)

func WithObjectives(costs CostReader, telemetry TelemetryReader) Option {
	return func(r *Resolver) { r.costs, r.telemetry = costs, telemetry }
}

func NewResolver(policies PolicyReader, catalog ModelCatalog, subscriptions SubscriptionChecker, opts ...Option) *Resolver {
	r := &Resolver{policies: policies, catalog: catalog, subscriptions: subscriptions, now: time.Now}
	for _, opt := range opts {
		opt(r)
	}

	return r
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

	if objectiveEnabled(policy.Objectives) {
		if err := r.score(ctx, in, policy, &decision); err != nil {
			return failedWithPolicy(in.RequestedModel, policy, domain.RoutingReasonPolicyUnavailable), err
		}
	}

	// Objective score wins when configured; weight and configured order remain
	// deterministic tie-breakers and preserve M1.2 behavior when disabled.
	sort.SliceStable(decision.Candidates, func(i, j int) bool {
		left, right := decision.Candidates[i], decision.Candidates[j]
		if left.Eligible != right.Eligible {
			return left.Eligible
		}

		if left.Eligible {
			if left.Score != nil && right.Score != nil && left.Score.TotalScore != right.Score.TotalScore {
				return left.Score.TotalScore > right.Score.TotalScore
			}

			if left.Weight != right.Weight {
				return left.Weight > right.Weight
			}
		}

		return left.Order < right.Order
	})

	applyExploration(in, policy, decision.Candidates)

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

	if err := validateObjectives(policy.Objectives); err != nil {
		return err
	}

	for i, candidate := range policy.Candidates {
		if candidate.Model == "" {
			return fmt.Errorf("routing policy: candidate %d model is required", i)
		}
	}

	return nil
}

func objectiveEnabled(o domain.RoutingObjectives) bool {
	return o.LatencyWeight > 0 || o.CostWeight > 0
}

func validateObjectives(o domain.RoutingObjectives) error {
	if o.ExplorationPermille > 1000 {
		return fmt.Errorf("routing policy: exploration_permille cannot exceed 1000")
	}

	if o.LatencyWeight > 0 && o.TargetLatencyMs == 0 {
		return fmt.Errorf("routing policy: target_latency_ms is required when latency_weight is set")
	}

	if o.CostWeight > 0 && o.TargetCostMicrousd == 0 {
		return fmt.Errorf("routing policy: target_cost_microusd is required when cost_weight is set")
	}

	if o.CostWeight > 0 && o.EstimatedInputTokens == 0 && o.EstimatedOutputTokens == 0 {
		return fmt.Errorf("routing policy: estimated token volume is required when cost_weight is set")
	}

	return nil
}

const neutralScore = 0.5

func (r *Resolver) score(ctx context.Context, in Input, policy *domain.RoutingPolicy, decision *domain.ModelRoutingDecision) error {
	o := policy.Objectives

	minSamples := o.MinTelemetrySamples
	if minSamples == 0 {
		minSamples = 5
	}

	maxAge := time.Duration(o.TelemetryMaxAgeSeconds) * time.Second
	if maxAge == 0 {
		maxAge = 5 * time.Minute
	}

	for i := range decision.Candidates {
		candidate := &decision.Candidates[i]
		if !candidate.Eligible {
			continue
		}

		explanation := &domain.RoutingScoreExplanation{
			LatencySource: domain.RoutingSignalNotConfigured, LatencyScore: neutralScore,
			CostSource: domain.RoutingSignalNotConfigured, CostScore: neutralScore,
		}

		if o.LatencyWeight > 0 {
			explanation.LatencySource = domain.RoutingSignalMissing
			if r.telemetry != nil {
				snapshots, err := r.telemetry.ForModel(ctx, candidate.Model, in.Group)
				if err != nil {
					return fmt.Errorf("resolve candidate %q telemetry: %w", candidate.Model, err)
				}

				applyTelemetry(explanation, snapshots, r.now(), maxAge, minSamples, o.TargetLatencyMs)
			}
		}

		if o.CostWeight > 0 {
			explanation.CostSource = domain.RoutingSignalMissing
			if r.costs != nil {
				profile, err := r.costs.GetActive(ctx, candidate.ModelServiceID)
				if err != nil {
					return fmt.Errorf("resolve candidate %q cost: %w", candidate.Model, err)
				}

				if profile != nil {
					explanation.CostSource = domain.RoutingSignalConfigured
					explanation.CostProfile = &profile.Ref
					explanation.EstimatedMicrousd = estimatedCost(profile, o.EstimatedInputTokens, o.EstimatedOutputTokens)
					explanation.CostScore = ratioScore(float64(o.TargetCostMicrousd), float64(explanation.EstimatedMicrousd))
				}
			}
		}

		weighted := float64(o.LatencyWeight)*explanation.LatencyScore + float64(o.CostWeight)*explanation.CostScore
		explanation.TotalScore = weighted / float64(o.LatencyWeight+o.CostWeight)
		candidate.Score = explanation
	}

	return nil
}

func applyTelemetry(out *domain.RoutingScoreExplanation, snapshots []EndpointTelemetry, now time.Time, maxAge time.Duration, minSamples uint32, target uint32) {
	var latency, success float64

	var samples uint32

	var latest time.Time

	staleSeen := false
	for _, snapshot := range snapshots {
		if snapshot.SampleCount < minSamples {
			continue
		}

		if snapshot.Updated.IsZero() || now.Sub(snapshot.Updated) > maxAge {
			staleSeen = true

			continue
		}

		latency += snapshot.LatencyMs * float64(snapshot.SampleCount)
		success += snapshot.SuccessRate * float64(snapshot.SampleCount)
		samples += snapshot.SampleCount

		if snapshot.Updated.After(latest) {
			latest = snapshot.Updated
		}
	}

	if samples == 0 {
		if staleSeen {
			out.LatencySource = domain.RoutingSignalStale
		}

		return
	}

	out.LatencySource = domain.RoutingSignalObserved
	out.LatencyMs = latency / float64(samples)
	out.SuccessRate = clamp01(success / float64(samples))
	out.TelemetrySamples = samples
	out.TelemetryUpdatedAt = latest.UTC().Format(time.RFC3339Nano)
	out.LatencyScore = ratioScore(float64(target), out.LatencyMs) * out.SuccessRate
}

func estimatedCost(profile *domain.RoutingCostProfile, inputTokens, outputTokens uint32) uint64 {
	cost := (float64(profile.InputMicrousdPerMillionToken)*float64(inputTokens) +
		float64(profile.OutputMicrousdPerMillionToken)*float64(outputTokens)) / 1_000_000

	return uint64(math.Ceil(cost))
}

func ratioScore(target, actual float64) float64 {
	if actual <= 0 {
		return 1
	}

	return clamp01(target / actual)
}

func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}

	if value > 1 {
		return 1
	}

	return value
}

func applyExploration(in Input, policy *domain.RoutingPolicy, candidates []domain.RoutingCandidateDecision) {
	if policy.Objectives.ExplorationPermille == 0 || in.DecisionKey == "" {
		return
	}

	eligible := 0
	for eligible < len(candidates) && candidates[eligible].Eligible {
		eligible++
	}

	if eligible < 2 {
		return
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(policy.Ref.ID + "\x00" + strconv.FormatUint(policy.Ref.Version, 10) + "\x00" + in.AccountID + "\x00" + in.DecisionKey))

	value := h.Sum64()
	if value%1000 >= uint64(policy.Objectives.ExplorationPermille) {
		return
	}

	selected := 1 + int((value/1000)%uint64(eligible-1))
	picked := candidates[selected]
	copy(candidates[1:selected+1], candidates[0:selected])

	candidates[0] = picked
	if candidates[0].Score != nil {
		candidates[0].Score.ExplorationChosen = true
	}
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
