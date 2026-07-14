package domain

import (
	"fmt"
	"math"
)

// RoutingScopeKind identifies where a virtual-model policy is owned. Project
// is part of the contract but is not active until identity/RBAC provides a
// trusted project ID; callers must never accept it directly from a header.
type RoutingScopeKind string

const (
	RoutingScopeGlobal  RoutingScopeKind = "global"
	RoutingScopeAccount RoutingScopeKind = "account"
	RoutingScopeProject RoutingScopeKind = "project"
)

// RoutingScope identifies one policy owner. Global has an empty ID.
type RoutingScope struct {
	Kind RoutingScopeKind `json:"kind"`
	ID   string           `json:"id,omitempty"`
}

// RoutingPolicyRef is the immutable policy identity attached to a decision.
// Updating or rolling back a policy always creates a new Version.
type RoutingPolicyRef struct {
	ID      string       `json:"id"`
	Version uint64       `json:"version"`
	Scope   RoutingScope `json:"scope"`
}

// RoutingPolicy is one immutable policy snapshot loaded on the request path.
type RoutingPolicy struct {
	Ref          RoutingPolicyRef         `json:"ref"`
	VirtualModel string                   `json:"virtual_model"`
	MaxAttempts  int                      `json:"max_attempts,omitempty"`
	Constraints  RoutingConstraints       `json:"constraints,omitempty"`
	Objectives   RoutingObjectives        `json:"objectives,omitempty"`
	Candidates   []RoutingPolicyCandidate `json:"candidates"`
}

// RoutingObjectives configures bounded, explainable optimization after hard
// constraints have run. Weights are relative (not percentages); both zero
// preserves the M1.2 weight-only ordering.
type RoutingObjectives struct {
	LatencyWeight          uint32 `json:"latency_weight,omitempty"`
	CostWeight             uint32 `json:"cost_weight,omitempty"`
	TargetLatencyMs        uint32 `json:"target_latency_ms,omitempty"`
	TargetCostMicrousd     uint64 `json:"target_cost_microusd,omitempty"`
	EstimatedInputTokens   uint32 `json:"estimated_input_tokens,omitempty"`
	EstimatedOutputTokens  uint32 `json:"estimated_output_tokens,omitempty"`
	MinTelemetrySamples    uint32 `json:"min_telemetry_samples,omitempty"`
	TelemetryMaxAgeSeconds uint32 `json:"telemetry_max_age_seconds,omitempty"`
	ExplorationPermille    uint32 `json:"exploration_permille,omitempty"`
}

// RoutingCostProfileRef identifies an immutable routing-only cost snapshot.
// It is intentionally distinct from customer pricing and billing rules.
type RoutingCostProfileRef struct {
	ID      string `json:"id"`
	Version uint64 `json:"version"`
}

type RoutingCostProfile struct {
	Ref                           RoutingCostProfileRef `json:"ref"`
	ModelServiceID                int64                 `json:"model_service_id"`
	InputMicrousdPerMillionToken  uint64                `json:"input_microusd_per_million_tokens"`
	OutputMicrousdPerMillionToken uint64                `json:"output_microusd_per_million_tokens"`
}

type RoutingSignalSource string

const (
	RoutingSignalNotConfigured RoutingSignalSource = "not_configured"
	RoutingSignalObserved      RoutingSignalSource = "observed"
	RoutingSignalConfigured    RoutingSignalSource = "configured"
	RoutingSignalMissing       RoutingSignalSource = "missing_neutral"
	RoutingSignalStale         RoutingSignalSource = "stale_neutral"
)

// RoutingScoreExplanation contains every dynamic input used by objective
// scoring. Scores are bounded to [0,1].
type RoutingScoreExplanation struct {
	LatencySource      RoutingSignalSource    `json:"latency_source"`
	LatencyMs          float64                `json:"latency_ms,omitempty"`
	SuccessRate        float64                `json:"success_rate,omitempty"`
	TelemetrySamples   uint32                 `json:"telemetry_samples,omitempty"`
	TelemetryUpdatedAt string                 `json:"telemetry_updated_at,omitempty"`
	LatencyScore       float64                `json:"latency_score"`
	CostSource         RoutingSignalSource    `json:"cost_source"`
	EstimatedMicrousd  uint64                 `json:"estimated_microusd,omitempty"`
	CostProfile        *RoutingCostProfileRef `json:"cost_profile,omitempty"`
	CostScore          float64                `json:"cost_score"`
	TotalScore         float64                `json:"total_score"`
	ExplorationChosen  bool                   `json:"exploration_chosen,omitempty"`
}

// RoutingConstraints are policy-wide hard filters. Empty fields do not
// constrain candidates; deny always wins over allow.
type RoutingConstraints struct {
	Regions     []string   `json:"regions,omitempty"`
	Modalities  []Modality `json:"modalities,omitempty"`
	AllowModels []string   `json:"allow_models,omitempty"`
	DenyModels  []string   `json:"deny_models,omitempty"`
}

// RoutingPolicyCandidate is a concrete model configured by a route policy.
// Candidate constraints narrow the policy-wide constraints and never widen
// subscriptions or allow lists.
type RoutingPolicyCandidate struct {
	Model      string     `json:"model"`
	Weight     uint32     `json:"weight,omitempty"`
	Regions    []string   `json:"regions,omitempty"`
	Modalities []Modality `json:"modalities,omitempty"`
}

// RoutingCandidateSource explains why a model entered candidate evaluation.
type RoutingCandidateSource string

const (
	RoutingCandidateRequested    RoutingCandidateSource = "requested_model"
	RoutingCandidatePolicy       RoutingCandidateSource = "route_policy"
	RoutingCandidateLegacyHeader RoutingCandidateSource = "legacy_fallback_header"
)

// RoutingOutcome is deliberately small so it is safe as a metric label.
type RoutingOutcome string

const (
	RoutingOutcomeResolved          RoutingOutcome = "resolved"
	RoutingOutcomeRejected          RoutingOutcome = "rejected"
	RoutingOutcomeDependencyFailure RoutingOutcome = "dependency_failure"
)

// RoutingReasonCode is a stable, non-secret explanation suitable for audit,
// traces, and bounded-cardinality metrics. Human-readable messages are not
// part of this contract.
type RoutingReasonCode string

const (
	RoutingReasonConcreteModel             RoutingReasonCode = "concrete_model"
	RoutingReasonVirtualPolicyMatched      RoutingReasonCode = "virtual_policy_matched"
	RoutingReasonLegacyFallbackAccepted    RoutingReasonCode = "legacy_fallback_accepted"
	RoutingReasonLegacyFallbackIgnored     RoutingReasonCode = "legacy_fallback_ignored"
	RoutingReasonCandidateNotFound         RoutingReasonCode = "candidate_not_found"
	RoutingReasonCandidateNotSubscribed    RoutingReasonCode = "candidate_not_subscribed"
	RoutingReasonCandidateRegionMismatch   RoutingReasonCode = "candidate_region_mismatch"
	RoutingReasonCandidateModalityMismatch RoutingReasonCode = "candidate_modality_mismatch"
	RoutingReasonCandidateDenied           RoutingReasonCode = "candidate_denied"
	RoutingReasonCandidateNotAllowed       RoutingReasonCode = "candidate_not_allowed"
	RoutingReasonNoPolicy                  RoutingReasonCode = "virtual_model_policy_not_found"
	RoutingReasonNoEligibleCandidate       RoutingReasonCode = "no_eligible_candidate"
	RoutingReasonPolicyInvalid             RoutingReasonCode = "routing_policy_invalid"
	RoutingReasonPolicyUnavailable         RoutingReasonCode = "routing_policy_unavailable"
)

// RoutingCandidateDecision records both accepted and rejected candidates. It
// never contains credentials, prompt content, or a free-form rejection message.
type RoutingCandidateDecision struct {
	ModelServiceID int64                    `json:"model_service_id,omitempty"`
	Model          string                   `json:"model"`
	Source         RoutingCandidateSource   `json:"source"`
	Eligible       bool                     `json:"eligible"`
	Reason         RoutingReasonCode        `json:"reason"`
	Order          int                      `json:"order"`
	Weight         uint32                   `json:"weight,omitempty"`
	Score          *RoutingScoreExplanation `json:"score,omitempty"`
}

// ModelRoutingDecision is produced before dispatch. Eligible candidates in
// Order become dispatch.Input.ModelChain; dispatch remains responsible for
// endpoint selection, retry, fallback execution, and attempt accounting.
type ModelRoutingDecision struct {
	RequestedModel string                     `json:"requested_model"`
	VirtualModel   bool                       `json:"virtual_model"`
	Outcome        RoutingOutcome             `json:"outcome"`
	Reason         RoutingReasonCode          `json:"reason"`
	Policy         *RoutingPolicyRef          `json:"policy,omitempty"`
	Candidates     []RoutingCandidateDecision `json:"candidates"`
	MaxAttempts    int                        `json:"max_attempts,omitempty"`
}

// EligibleModels returns the ordered model chain supplied to dispatch.
func (d ModelRoutingDecision) EligibleModels() []string {
	models := make([]string, 0, len(d.Candidates))
	for _, candidate := range d.Candidates {
		if candidate.Eligible {
			models = append(models, candidate.Model)
		}
	}

	return models
}

// Validate catches malformed resolver output before it reaches dispatch.
func (d ModelRoutingDecision) Validate() error {
	if d.RequestedModel == "" {
		return fmt.Errorf("routing decision: requested model is required")
	}

	if d.Outcome == "" || d.Reason == "" {
		return fmt.Errorf("routing decision: outcome and reason are required")
	}

	if d.VirtualModel && d.Outcome == RoutingOutcomeResolved && d.Policy == nil {
		return fmt.Errorf("routing decision: resolved virtual model requires a policy")
	}

	if d.Policy != nil && (d.Policy.ID == "" || d.Policy.Version == 0) {
		return fmt.Errorf("routing decision: policy ID and version are required")
	}

	for i, candidate := range d.Candidates {
		if candidate.Model == "" || candidate.Source == "" || candidate.Reason == "" {
			return fmt.Errorf("routing decision: candidate %d is incomplete", i)
		}

		if candidate.Score != nil {
			score := candidate.Score
			if score.LatencySource == "" || score.CostSource == "" ||
				!boundedScore(score.LatencyScore) || !boundedScore(score.CostScore) || !boundedScore(score.TotalScore) {
				return fmt.Errorf("routing decision: candidate %d score is invalid", i)
			}

			if score.CostProfile != nil && (score.CostProfile.ID == "" || score.CostProfile.Version == 0) {
				return fmt.Errorf("routing decision: candidate %d cost profile is invalid", i)
			}
		}
	}

	if d.Outcome == RoutingOutcomeResolved && len(d.EligibleModels()) == 0 {
		return fmt.Errorf("routing decision: resolved outcome has no eligible candidates")
	}

	return nil
}

func boundedScore(score float64) bool {
	return !math.IsNaN(score) && !math.IsInf(score, 0) && score >= 0 && score <= 1
}
