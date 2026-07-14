package moderation

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/policy"
)

type engineFunc func(context.Context, policy.EvaluationInput) (policy.Decision, error)

func (f engineFunc) Evaluate(ctx context.Context, input policy.EvaluationInput) (policy.Decision, error) {
	return f(ctx, input)
}

func policyDecision(action policy.Action) policy.Decision {
	decision := policy.Decision{
		Action: action,
		Policy: policy.PolicyRef{ID: "test", Version: 1, Scope: policy.Scope{Kind: policy.ScopeGlobal}},
		RuleID: "test-rule", ReasonCode: "test-reason",
	}
	if action == policy.ActionRedact {
		decision.Mutations = []policy.Mutation{{ID: "mask", Kind: policy.MutationRedact, Target: "body", Replacement: []byte("masked")}}
	}

	return decision
}

func TestLegacyEngineCompatibility(t *testing.T) {
	if NewLegacyEngine(nil) != nil {
		t.Fatal("nil moderator should produce nil engine")
	}

	blocked := errors.New("legacy blocked")
	engine := NewLegacyEngine(stubGuard{inErr: blocked, outErr: blocked})
	inputDecision, err := engine.Evaluate(context.Background(), policy.EvaluationInput{
		Stage: policy.StageInput, Request: &domain.RequestEnvelope{RawBytes: []byte("secret")},
	})
	if err != nil || inputDecision.Action != policy.ActionDeny || !errors.Is(inputDecision.Cause, blocked) {
		t.Fatalf("input decision=%+v err=%v", inputDecision, err)
	}
	outputDecision, err := engine.Evaluate(context.Background(), policy.EvaluationInput{
		Stage: policy.StageOutput, Content: policy.Content{Bytes: []byte("secret")},
	})
	if err != nil || outputDecision.Action != policy.ActionDeny || !errors.Is(outputDecision.Cause, blocked) {
		t.Fatalf("output decision=%+v err=%v", outputDecision, err)
	}
	if _, err := engine.Evaluate(context.Background(), policy.EvaluationInput{Stage: "other"}); err == nil {
		t.Fatal("unsupported stage succeeded")
	}

	allow, err := NewLegacyEngine(stubGuard{}).Evaluate(context.Background(), policy.EvaluationInput{Stage: policy.StageInput})
	if err != nil || allow.Action != policy.ActionAllow {
		t.Fatalf("allow=%+v err=%v", allow, err)
	}
}

func TestLegacyEngineAttributesDecisionToResolvedPolicy(t *testing.T) {
	selected := policy.PolicyRef{ID: "pii", Version: 3, Scope: policy.Scope{Kind: policy.ScopeAccount, ID: "acct"}}
	decision, err := NewLegacyEngine(stubGuard{}).Evaluate(context.Background(), policy.EvaluationInput{
		Stage: policy.StageInput, Policy: &selected,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Policy != selected {
		t.Fatalf("policy = %+v, want selected policy %+v", decision.Policy, selected)
	}
}

func TestPolicyModeratorEnforcesAndRecordsDecisions(t *testing.T) {
	if NewPolicyModerator(nil, policy.EvaluationInput{}, nil) != nil {
		t.Fatal("nil engine should produce nil moderator")
	}

	var audits []policy.AuditRecord
	engine := engineFunc(func(_ context.Context, input policy.EvaluationInput) (policy.Decision, error) {
		if bytes.Contains(input.Content.Bytes, []byte("deny")) {
			return policyDecision(policy.ActionDeny), nil
		}

		return policyDecision(policy.ActionAllow), nil
	})
	moderator := NewPolicyModerator(engine, policy.EvaluationInput{}, func(record policy.AuditRecord) {
		audits = append(audits, record)
	})
	if err := moderator.CheckInput(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := moderator.CheckOutput(context.Background(), []byte("allow")); err != nil {
		t.Fatal(err)
	}
	if err := moderator.CheckOutput(context.Background(), []byte("deny")); !errors.Is(err, policy.ErrDenied) {
		t.Fatalf("deny error=%v", err)
	}
	if len(audits) != 3 || audits[0].Stage != policy.StageInput || audits[1].Stage != policy.StageOutput {
		t.Fatalf("audits=%+v", audits)
	}
}

func TestPolicyModeratorFailsClosed(t *testing.T) {
	tests := map[string]engineFunc{
		"engine error": func(context.Context, policy.EvaluationInput) (policy.Decision, error) {
			return policy.Decision{}, errors.New("unavailable")
		},
		"invalid decision": func(context.Context, policy.EvaluationInput) (policy.Decision, error) {
			return policy.Decision{}, nil
		},
		"redact unsupported": func(context.Context, policy.EvaluationInput) (policy.Decision, error) {
			return policyDecision(policy.ActionRedact), nil
		},
	}

	for name, engine := range tests {
		t.Run(name, func(t *testing.T) {
			moderator := NewPolicyModerator(engine, policy.EvaluationInput{}, nil)
			if err := moderator.CheckOutput(context.Background(), []byte("content")); err == nil {
				t.Fatal("CheckOutput succeeded")
			}
		})
	}
}
