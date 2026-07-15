package moderation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/policy"
)

var moderatorPolicyRef = policy.PolicyRef{
	ID: "builtin/moderator", Version: 1,
	Scope: policy.Scope{Kind: policy.ScopeGlobal},
}

const reasonModeratorRejected = "moderator_rejected"

// ModeratorEngine adapts the error-only Moderator contract to explicit policy
// decisions. Detector errors are reduced to a stable, non-secret reason code.
type ModeratorEngine struct{ moderator Moderator }

func NewModeratorEngine(moderator Moderator) policy.Engine {
	if moderator == nil {
		return nil
	}

	return &ModeratorEngine{moderator: moderator}
}

func (e *ModeratorEngine) Evaluate(ctx context.Context, input policy.EvaluationInput) (policy.Decision, error) {
	decision := policy.Decision{
		Action: policy.ActionAllow, Policy: moderatorPolicyRef,
		RuleID: "moderator", ReasonCode: "moderator_allowed",
	}
	if input.Policy != nil {
		decision.Policy = *input.Policy
	}

	var err error
	switch input.Stage {
	case policy.StageInput:
		err = e.moderator.CheckInput(ctx, input.Request)
	case policy.StageOutput:
		err = e.moderator.CheckOutput(ctx, input.Content.Bytes)
	default:
		return policy.Decision{}, fmt.Errorf("moderator: unsupported stage %q", input.Stage)
	}

	if err != nil {
		decision.Action = policy.ActionDeny
		decision.ReasonCode = reasonModeratorRejected
	}

	return decision, nil
}

// PolicyModerator lets the existing response-stream decorator enforce a new
// policy.Engine without duplicating streaming orchestration.
type PolicyModerator struct {
	engine         policy.Engine
	base           policy.EvaluationInput
	record         func(policy.AuditRecord)
	adapter        policy.DocumentAdapter
	mode           policy.OutputMode
	maxBufferBytes int
	decoded        outputTextAccumulator
}

type PolicyModeratorOption func(*PolicyModerator)

func WithDocumentAdapter(adapter policy.DocumentAdapter) PolicyModeratorOption {
	return func(m *PolicyModerator) { m.adapter = adapter }
}

func WithOutputMode(mode policy.OutputMode, maxBufferBytes int) PolicyModeratorOption {
	return func(m *PolicyModerator) {
		m.mode = mode
		m.maxBufferBytes = maxBufferBytes
	}
}

func NewPolicyModerator(engine policy.Engine, base policy.EvaluationInput, record func(policy.AuditRecord), opts ...PolicyModeratorOption) Moderator {
	if engine == nil {
		return nil
	}

	m := &PolicyModerator{
		engine: engine, base: base, record: record,
		adapter: policy.JSONDocumentAdapter{}, mode: policy.OutputBestEffortStreaming,
		maxBufferBytes: policy.DefaultMaxBufferBytes,
	}
	for _, opt := range opts {
		opt(m)
	}

	window := policy.DefaultStreamingWindowBytes
	if m.maxBufferBytes > 0 && m.maxBufferBytes < window {
		window = m.maxBufferBytes
	}

	m.decoded.maxBytes = window

	return m
}

func (m *PolicyModerator) CheckInput(ctx context.Context, _ *domain.RequestEnvelope) error {
	input := m.base
	input.Stage = policy.StageInput

	return m.evaluate(ctx, input)
}

func (m *PolicyModerator) CheckOutput(ctx context.Context, chunk []byte) error {
	_, err := m.EnforceOutput(ctx, chunk, false)

	return err
}

func (m *PolicyModerator) OutputMode() policy.OutputMode { return m.mode }
func (m *PolicyModerator) MaxBufferBytes() int           { return m.maxBufferBytes }

func (m *PolicyModerator) RecordOutputFailure(reasonCode string) {
	ref := moderatorPolicyRef
	if m.base.Policy != nil {
		ref = *m.base.Policy
	}

	m.recordAudit(policy.AuditRecord{
		Stage: policy.StageOutput, Action: policy.ActionDeny, Policy: ref,
		RuleID: "gateway_output_enforcement", ReasonCode: reasonCode,
		Enforcement: policy.EnforcementFailed,
	})
}

func (m *PolicyModerator) EnforceOutput(ctx context.Context, content []byte, final bool) ([]byte, error) {
	input := m.base
	input.Stage = policy.StageOutput
	input.Content.Streaming = !final

	var protocol domain.Protocol

	var modality domain.Modality
	if input.Request != nil {
		protocol = input.Request.SourceProtocol
		modality = input.Request.Modality
	}

	if m.mode == policy.OutputBestEffortStreaming && !final {
		decoded := m.decoded.Push(content)
		if len(decoded) == 0 {
			return content, nil
		}

		input.Content.MediaType = "text/plain"
		input.Content.Bytes = decoded
		input.Segments = []policy.TextSegment{{Target: "/text", Text: decoded}}
	} else {
		input.Content.Bytes = content

		segments, err := m.adapter.Extract(content, protocol, modality)
		if err == nil {
			input.Segments = segments
		}
	}

	decision, err := m.engine.Evaluate(ctx, input)
	if err != nil {
		m.RecordOutputFailure("engine_unavailable")
		return nil, fmt.Errorf("policy engine unavailable: %w", err)
	}

	if err := decision.Validate(); err != nil {
		m.RecordOutputFailure("invalid_decision")
		return nil, err
	}

	audit := decision.SafeAudit(policy.StageOutput)
	switch decision.Action {
	case policy.ActionAllow:
		m.recordAudit(audit.WithEnforcement(policy.EnforcementAllowed))

		return content, nil
	case policy.ActionDeny:
		m.recordAudit(audit.WithEnforcement(policy.EnforcementDenied))
		return nil, policy.ErrDenied
	case policy.ActionRedact:
		if m.mode != policy.OutputStrictBuffered || !final {
			m.recordAudit(audit.WithEnforcement(policy.EnforcementFailed))

			return nil, policy.ErrRedactionUnsupported
		}

		rebuilt, applyErr := m.adapter.Apply(content, protocol, modality, decision.Mutations)
		if applyErr != nil {
			m.recordAudit(audit.WithEnforcement(policy.EnforcementFailed))

			return nil, applyErr
		}

		m.recordAudit(audit.WithEnforcement(policy.EnforcementApplied))

		return rebuilt, nil
	default:
		return nil, fmt.Errorf("policy: unsupported action %q", decision.Action)
	}
}

func (m *PolicyModerator) recordAudit(record policy.AuditRecord) {
	if m.record != nil {
		m.record(record)
	}
}

func (m *PolicyModerator) evaluate(ctx context.Context, input policy.EvaluationInput) error {
	decision, err := m.engine.Evaluate(ctx, input)
	if err != nil {
		if input.Stage == policy.StageOutput {
			m.RecordOutputFailure("engine_unavailable")
		}

		return fmt.Errorf("policy engine unavailable: %w", err)
	}

	if err := decision.Validate(); err != nil {
		if input.Stage == policy.StageOutput {
			m.RecordOutputFailure("invalid_decision")
		}

		return err
	}

	switch decision.Action {
	case policy.ActionAllow:
		m.recordAudit(decision.SafeAudit(input.Stage).WithEnforcement(policy.EnforcementAllowed))
		return nil
	case policy.ActionDeny:
		m.recordAudit(decision.SafeAudit(input.Stage).WithEnforcement(policy.EnforcementDenied))
		return policy.ErrDenied
	case policy.ActionRedact:
		m.recordAudit(decision.SafeAudit(input.Stage).WithEnforcement(policy.EnforcementFailed))
		return policy.ErrRedactionUnsupported
	default:
		return fmt.Errorf("policy: unsupported action %q", decision.Action)
	}
}

type outputTextAccumulator struct {
	pending  []byte
	text     []byte
	maxBytes int
}

func (a *outputTextAccumulator) Push(chunk []byte) []byte {
	standaloneJSON := len(a.pending) == 0 && json.Valid(chunk)
	plain := len(a.pending) == 0 && !bytes.Contains(chunk, []byte("data:")) && !standaloneJSON
	a.pending = append(a.pending, chunk...)

	a.pending = bytes.ReplaceAll(a.pending, []byte("\r\n"), []byte("\n"))
	for {
		index := bytes.Index(a.pending, []byte("\n\n"))
		if index < 0 {
			break
		}

		frame := a.pending[:index]
		a.pending = a.pending[index+2:]

		for _, line := range bytes.Split(frame, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}

			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}

			a.text = append(a.text, extractJSONText(payload)...)
		}
	}

	if standaloneJSON {
		a.text = append(a.text, extractJSONText(chunk)...)
		a.pending = a.pending[:0]
	}

	if plain {
		a.text = append(a.text, chunk...)
		a.pending = a.pending[:0]
	}

	limit := a.maxBytes
	if limit <= 0 {
		limit = policy.DefaultStreamingWindowBytes
	}

	if len(a.pending) > limit {
		a.text = append(a.text, a.pending...)
		a.pending = a.pending[:0]
	}

	if len(a.text) > limit {
		tail := make([]byte, limit)
		copy(tail, a.text[len(a.text)-limit:])
		a.text = tail
	}

	return append([]byte(nil), a.text...)
}

func extractJSONText(payload []byte) []byte {
	var value any
	if json.Unmarshal(payload, &value) != nil {
		return nil
	}

	var parts []string

	var walk func(any, string)

	walk = func(node any, key string) {
		switch typed := node.(type) {
		case map[string]any:
			for childKey, child := range typed {
				walk(child, childKey)
			}
		case []any:
			for _, child := range typed {
				walk(child, key)
			}
		case string:
			if (key == "text" || key == "content" || key == "delta" || key == "output_text") && typed != "" {
				parts = append(parts, typed)
			}
		}
	}
	walk(value, "")

	return []byte(strings.Join(parts, ""))
}

var _ policy.Engine = (*ModeratorEngine)(nil)
var _ Moderator = (*PolicyModerator)(nil)
