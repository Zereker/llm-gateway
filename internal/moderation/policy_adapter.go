package moderation

import (
	"context"
	"fmt"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/policy"
)

var legacyPolicyRef = policy.PolicyRef{
	ID: "builtin/legacy-moderator", Version: 1,
	Scope: policy.Scope{Kind: policy.ScopeGlobal},
}

// LegacyEngine adapts the error-only Moderator contract to explicit policy
// decisions. Existing moderator errors remain available only as Decision.Cause
// and are never included in SafeAudit.
type LegacyEngine struct{ moderator Moderator }

func NewLegacyEngine(moderator Moderator) policy.Engine {
	if moderator == nil {
		return nil
	}

	return &LegacyEngine{moderator: moderator}
}

func (e *LegacyEngine) Evaluate(ctx context.Context, input policy.EvaluationInput) (policy.Decision, error) {
	decision := policy.Decision{
		Action: policy.ActionAllow, Policy: legacyPolicyRef,
		RuleID: "legacy_moderator", ReasonCode: "legacy_moderator_allowed",
	}

	var err error
	switch input.Stage {
	case policy.StageInput:
		err = e.moderator.CheckInput(ctx, input.Request)
	case policy.StageOutput:
		err = e.moderator.CheckOutput(ctx, input.Content.Bytes)
	default:
		return policy.Decision{}, fmt.Errorf("legacy moderator: unsupported stage %q", input.Stage)
	}

	if err != nil {
		decision.Action = policy.ActionDeny
		decision.ReasonCode = "legacy_moderator_rejected"
		decision.Cause = err
	}

	return decision, nil
}

// PolicyModerator lets the existing response-stream decorator enforce a new
// policy.Engine without duplicating streaming orchestration.
type PolicyModerator struct {
	engine policy.Engine
	base   policy.EvaluationInput
	record func(policy.AuditRecord)
}

func NewPolicyModerator(engine policy.Engine, base policy.EvaluationInput, record func(policy.AuditRecord)) Moderator {
	if engine == nil {
		return nil
	}

	return &PolicyModerator{engine: engine, base: base, record: record}
}

func (m *PolicyModerator) CheckInput(ctx context.Context, _ *domain.RequestEnvelope) error {
	input := m.base
	input.Stage = policy.StageInput

	return m.evaluate(ctx, input)
}

func (m *PolicyModerator) CheckOutput(ctx context.Context, chunk []byte) error {
	input := m.base
	input.Stage = policy.StageOutput
	input.Content.Bytes = chunk

	return m.evaluate(ctx, input)
}

func (m *PolicyModerator) evaluate(ctx context.Context, input policy.EvaluationInput) error {
	decision, err := m.engine.Evaluate(ctx, input)
	if err != nil {
		return fmt.Errorf("policy engine unavailable: %w", err)
	}

	if err := decision.Validate(); err != nil {
		return err
	}

	if m.record != nil {
		m.record(decision.SafeAudit(input.Stage))
	}

	switch decision.Action {
	case policy.ActionAllow:
		return nil
	case policy.ActionDeny:
		if decision.Cause != nil {
			return decision.Cause
		}

		return policy.ErrDenied
	case policy.ActionRedact:
		return policy.ErrRedactionUnsupported
	default:
		return fmt.Errorf("policy: unsupported action %q", decision.Action)
	}
}

var _ policy.Engine = (*LegacyEngine)(nil)
var _ Moderator = (*PolicyModerator)(nil)
