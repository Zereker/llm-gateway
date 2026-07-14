package domain

import "fmt"

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
	Candidates   []RoutingPolicyCandidate `json:"candidates"`
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
	ModelServiceID int64                  `json:"model_service_id,omitempty"`
	Model          string                 `json:"model"`
	Source         RoutingCandidateSource `json:"source"`
	Eligible       bool                   `json:"eligible"`
	Reason         RoutingReasonCode      `json:"reason"`
	Order          int                    `json:"order"`
	Weight         uint32                 `json:"weight,omitempty"`
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
	}

	if d.Outcome == RoutingOutcomeResolved && len(d.EligibleModels()) == 0 {
		return fmt.Errorf("routing decision: resolved outcome has no eligible candidates")
	}

	return nil
}
