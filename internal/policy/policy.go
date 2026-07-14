// Package policy defines the vendor-neutral Policy Enforcement contract.
// Engines decide; gateway middleware enforces. Detectors, DLP products, and
// provider-specific moderation clients remain replaceable implementations.
package policy

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/zereker/llm-gateway/internal/domain"
)

type Stage string

const (
	StageInput  Stage = "input"
	StageOutput Stage = "output"
)

type Action string

const (
	ActionAllow  Action = "allow"
	ActionDeny   Action = "deny"
	ActionRedact Action = "redact"
)

type OutputMode string

const (
	OutputDisabled            OutputMode = "disabled"
	OutputStrictBuffered      OutputMode = "strict_buffered"
	OutputBestEffortStreaming OutputMode = "best_effort_streaming"
)

const (
	DefaultMaxBufferBytes       = 4 << 20
	MaxBufferBytes              = 64 << 20
	DefaultStreamingWindowBytes = 64 << 10
)

type ScopeKind string

const (
	ScopeGlobal  ScopeKind = "global"
	ScopeAccount ScopeKind = "account"
	ScopeProject ScopeKind = "project"
	ScopeAPIKey  ScopeKind = "api_key"
)

type Scope struct {
	Kind ScopeKind `json:"kind"`
	ID   string    `json:"id,omitempty"`
}

type PolicyRef struct {
	ID      string `json:"id"`
	Version uint64 `json:"version"`
	Scope   Scope  `json:"scope"`
}

// Subject carries trusted identity resolved by the gateway. Engines must not
// derive scope from caller-controlled headers.
type Subject struct {
	AccountID string `json:"account_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	APIKeyID  string `json:"api_key_id,omitempty"`
}

// Content is runtime-only and deliberately excluded from JSON/audit output.
type Content struct {
	MediaType string `json:"media_type,omitempty"`
	Bytes     []byte `json:"-"`
	Streaming bool   `json:"streaming,omitempty"`
}

// TextSegment is a protocol adapter's runtime-only view of a mutable text
// node. Target is an RFC 6901 JSON Pointer into the client-facing document.
type TextSegment struct {
	Target string `json:"target"`
	Text   []byte `json:"-"`
}

type EvaluationInput struct {
	Stage    Stage           `json:"stage"`
	Subject  Subject         `json:"subject"`
	Model    string          `json:"model,omitempty"`
	Modality domain.Modality `json:"modality,omitempty"`
	Content  Content         `json:"content"`
	Segments []TextSegment   `json:"-"`
	Policy   *PolicyRef      `json:"policy,omitempty"`

	// Request preserves the typed, protocol-neutral envelope for legacy
	// adapters. New engines should consume Content and typed metadata.
	Request *domain.RequestEnvelope `json:"-"`
}

type MutationKind string

const MutationRedact MutationKind = "redact"

// Mutation contains an executable replacement, but Replacement is never
// serialized. AuditRecord exposes only stable mutation ID/kind/target.
type Mutation struct {
	ID          string       `json:"id"`
	Kind        MutationKind `json:"kind"`
	Target      string       `json:"target"`
	Replacement []byte       `json:"-"`
}

type Decision struct {
	Action     Action     `json:"action"`
	Policy     PolicyRef  `json:"policy"`
	RuleID     string     `json:"rule_id"`
	ReasonCode string     `json:"reason_code"`
	Mutations  []Mutation `json:"-"`

	// Cause exists only for compatibility/error attribution in-process. It is
	// excluded from JSON and SafeAudit because it may contain sensitive text.
	Cause error `json:"-"`
}

type AuditMutation struct {
	ID     string       `json:"id"`
	Kind   MutationKind `json:"kind"`
	Target string       `json:"target"`
}

type AuditRecord struct {
	Stage       Stage             `json:"stage"`
	Action      Action            `json:"action"`
	Policy      PolicyRef         `json:"policy"`
	RuleID      string            `json:"rule_id"`
	ReasonCode  string            `json:"reason_code"`
	Mutations   []AuditMutation   `json:"mutations,omitempty"`
	Enforcement EnforcementStatus `json:"enforcement"`
}

type EnforcementStatus string

const (
	EnforcementAllowed EnforcementStatus = "allowed"
	EnforcementDenied  EnforcementStatus = "denied"
	EnforcementApplied EnforcementStatus = "applied"
	EnforcementFailed  EnforcementStatus = "failed"
)

type Definition struct {
	Ref            PolicyRef  `json:"ref"`
	Name           string     `json:"name"`
	InputEnabled   bool       `json:"input_enabled"`
	OutputMode     OutputMode `json:"output_mode"`
	MaxBufferBytes int        `json:"max_buffer_bytes"`
}

type Resolver interface {
	Resolve(ctx context.Context, subject Subject) (*Definition, error)
}

type Engine interface {
	Evaluate(ctx context.Context, input EvaluationInput) (Decision, error)
}

func (r PolicyRef) Validate() error {
	if r.ID == "" || r.Version == 0 {
		return errors.New("policy reference: id and version are required")
	}

	switch r.Scope.Kind {
	case ScopeGlobal:
		if r.Scope.ID != "" {
			return errors.New("policy reference: global scope ID must be empty")
		}
	case ScopeAccount, ScopeProject, ScopeAPIKey:
		if r.Scope.ID == "" {
			return errors.New("policy reference: scoped policy ID is required")
		}
	default:
		return fmt.Errorf("policy reference: invalid scope %q", r.Scope.Kind)
	}

	return nil
}

var (
	ErrDenied               = errors.New("policy: denied")
	ErrRedactionUnsupported = errors.New("policy: redaction executor unavailable")
)

func (d Decision) Validate() error {
	if d.Action != ActionAllow && d.Action != ActionDeny && d.Action != ActionRedact {
		return fmt.Errorf("policy decision: invalid action %q", d.Action)
	}

	if err := d.Policy.Validate(); err != nil {
		return fmt.Errorf("policy decision: %w", err)
	}

	if d.RuleID == "" || d.ReasonCode == "" {
		return errors.New("policy decision: rule_id and reason_code are required")
	}

	if d.Action == ActionRedact && len(d.Mutations) == 0 {
		return errors.New("policy decision: redact requires mutations")
	}

	if d.Action != ActionRedact && len(d.Mutations) > 0 {
		return errors.New("policy decision: mutations require redact action")
	}

	for i, mutation := range d.Mutations {
		if mutation.ID == "" || mutation.Kind == "" || mutation.Target == "" {
			return fmt.Errorf("policy decision: mutation %d is incomplete", i)
		}
	}

	return nil
}

func (d Decision) SafeAudit(stage Stage) AuditRecord {
	mutations := make([]AuditMutation, 0, len(d.Mutations))
	for _, mutation := range d.Mutations {
		mutations = append(mutations, AuditMutation{ID: mutation.ID, Kind: mutation.Kind, Target: mutation.Target})
	}

	return AuditRecord{
		Stage: stage, Action: d.Action, Policy: d.Policy, RuleID: d.RuleID,
		ReasonCode: d.ReasonCode, Mutations: mutations,
	}
}

func (r AuditRecord) WithEnforcement(status EnforcementStatus) AuditRecord {
	r.Enforcement = status

	return r
}

func (d Definition) Validate() error {
	if err := d.Ref.Validate(); err != nil {
		return err
	}

	if d.Name == "" {
		return errors.New("policy definition: name is required")
	}

	switch d.OutputMode {
	case OutputDisabled, OutputStrictBuffered, OutputBestEffortStreaming:
	default:
		return fmt.Errorf("policy definition: invalid output mode %q", d.OutputMode)
	}

	if d.MaxBufferBytes < 0 {
		return errors.New("policy definition: max_buffer_bytes cannot be negative")
	}

	if d.MaxBufferBytes > MaxBufferBytes {
		return fmt.Errorf("policy definition: max_buffer_bytes exceeds %d", MaxBufferBytes)
	}

	return nil
}

type Binding struct {
	Policy  PolicyRef
	Enabled bool
}

// SelectBinding applies API key > project > account > global precedence.
// Within one scope the highest enabled immutable version wins.
func SelectBinding(bindings []Binding, subject Subject) *PolicyRef {
	matched := make([]PolicyRef, 0, len(bindings))
	for _, binding := range bindings {
		if binding.Enabled && binding.Policy.Validate() == nil && scopeMatches(binding.Policy.Scope, subject) {
			matched = append(matched, binding.Policy)
		}
	}

	if len(matched) == 0 {
		return nil
	}

	sort.SliceStable(matched, func(i, j int) bool {
		left, right := matched[i], matched[j]
		if scopeRank(left.Scope.Kind) != scopeRank(right.Scope.Kind) {
			return scopeRank(left.Scope.Kind) > scopeRank(right.Scope.Kind)
		}

		return left.Version > right.Version
	})

	selected := matched[0]

	return &selected
}

func scopeMatches(scope Scope, subject Subject) bool {
	switch scope.Kind {
	case ScopeGlobal:
		return scope.ID == ""
	case ScopeAccount:
		return scope.ID != "" && scope.ID == subject.AccountID
	case ScopeProject:
		return scope.ID != "" && scope.ID == subject.ProjectID
	case ScopeAPIKey:
		return scope.ID != "" && scope.ID == subject.APIKeyID
	default:
		return false
	}
}

func scopeRank(kind ScopeKind) int {
	switch kind {
	case ScopeAPIKey:
		return 4
	case ScopeProject:
		return 3
	case ScopeAccount:
		return 2
	case ScopeGlobal:
		return 1
	default:
		return 0
	}
}
