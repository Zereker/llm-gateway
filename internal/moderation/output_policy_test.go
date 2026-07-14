package moderation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/policy"
)

type passthroughPolicyStream struct{ final []byte }

func (s *passthroughPolicyStream) Feed(chunk []byte) ([]byte, error) { return chunk, nil }
func (s *passthroughPolicyStream) Flush() ([]byte, *domain.Usage, error) {
	return s.final, nil, nil
}

func TestStrictBufferedWithholdsAndRedactsCompleteJSON(t *testing.T) {
	engine := engineFunc(func(_ context.Context, input policy.EvaluationInput) (policy.Decision, error) {
		decision := policyDecision(policy.ActionAllow)
		if input.Stage == policy.StageOutput && strings.Contains(string(input.Content.Bytes), "4111") {
			decision = policyDecision(policy.ActionRedact)
			decision.Mutations[0].Target = "/choices/0/message/content"
			decision.Mutations[0].Replacement = []byte("card [MASKED]")
		}

		return decision, nil
	})
	base := policy.EvaluationInput{Request: &domain.RequestEnvelope{SourceProtocol: domain.ProtoOpenAI, Modality: domain.ModalityChat}}
	controller := NewPolicyModerator(engine, base, nil, WithOutputMode(policy.OutputStrictBuffered, 1024))
	ctx := ContextWithModerator(context.Background(), controller)
	stream := WrapStream(ctx, &passthroughPolicyStream{})

	out, err := stream.Feed([]byte(`{"choices":[{"message":{"content":"card 4111"}}]}`))
	if err != nil || len(out) != 0 {
		t.Fatalf("strict Feed out=%q err=%v", out, err)
	}
	final, _, err := stream.Flush()
	if err != nil || strings.Contains(string(final), "4111") || !strings.Contains(string(final), "[MASKED]") {
		t.Fatalf("strict Flush out=%s err=%v", final, err)
	}
}

func TestStrictBufferedFailsClosedOnLimit(t *testing.T) {
	var audits []policy.AuditRecord
	controller := NewPolicyModerator(engineFunc(func(context.Context, policy.EvaluationInput) (policy.Decision, error) {
		return policyDecision(policy.ActionAllow), nil
	}), policy.EvaluationInput{}, func(record policy.AuditRecord) { audits = append(audits, record) }, WithOutputMode(policy.OutputStrictBuffered, 4))
	stream := WrapStream(ContextWithModerator(context.Background(), controller), &passthroughPolicyStream{})
	if _, err := stream.Feed([]byte("12345")); err == nil {
		t.Fatal("buffer overflow succeeded")
	}
	if len(audits) != 1 || audits[0].Enforcement != policy.EnforcementFailed || audits[0].ReasonCode != "strict_buffer_limit_exceeded" {
		t.Fatalf("audits=%+v", audits)
	}
}

func TestBestEffortStreamingDetectsSSEFrameSplitAcrossTransportChunks(t *testing.T) {
	engine := engineFunc(func(_ context.Context, input policy.EvaluationInput) (policy.Decision, error) {
		if strings.Contains(string(input.Content.Bytes), "secret") {
			return policyDecision(policy.ActionDeny), nil
		}

		return policyDecision(policy.ActionAllow), nil
	})
	controller := NewPolicyModerator(engine, policy.EvaluationInput{}, nil,
		WithOutputMode(policy.OutputBestEffortStreaming, 0))
	stream := WrapStream(ContextWithModerator(context.Background(), controller), &passthroughPolicyStream{})
	if _, err := stream.Feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"sec")); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Feed([]byte("ret\"}}]}\n\n")); !errors.Is(err, policy.ErrDenied) {
		t.Fatalf("transport-split secret err=%v", err)
	}
}

func TestBestEffortStreamingDetectsTextSplitAcrossSSEFrames(t *testing.T) {
	engine := engineFunc(func(_ context.Context, input policy.EvaluationInput) (policy.Decision, error) {
		if strings.Contains(string(input.Content.Bytes), "secret") {
			return policyDecision(policy.ActionDeny), nil
		}

		return policyDecision(policy.ActionAllow), nil
	})
	controller := NewPolicyModerator(engine, policy.EvaluationInput{}, nil,
		WithOutputMode(policy.OutputBestEffortStreaming, 0))
	stream := WrapStream(ContextWithModerator(context.Background(), controller), &passthroughPolicyStream{})
	first := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"sec\"}}]}\n\n")
	second := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ret\"}}]}\n\n")
	if out, err := stream.Feed(first); err != nil || len(out) == 0 {
		t.Fatalf("first frame out=%q err=%v", out, err)
	}
	if _, err := stream.Feed(second); !errors.Is(err, policy.ErrDenied) {
		t.Fatalf("split secret err=%v", err)
	}
}
