package middleware

import (
	"bytes"
	"io"
	"strconv"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/metric"
	"github.com/zereker/llm-gateway/internal/moderation"
	"github.com/zereker/llm-gateway/internal/policy"
)

// Moderator is an alias for moderation.Moderator, kept for the old import path.
// New code should use internal/moderation.Moderator directly.
type Moderator = moderation.Moderator
type PolicyEngine = policy.Engine

// ModerationOption configures the Moderation middleware (same interface-Option pattern as otelgin v0.68.0).
type ModerationOption interface {
	apply(*moderationConfig)
}

type moderationOptionFunc func(*moderationConfig)

func (f moderationOptionFunc) apply(c *moderationConfig) { f(c) }

type moderationConfig struct {
	engine      PolicyEngine
	auditTracer AuditTracer
	resolver    policy.Resolver
	documents   policy.DocumentAdapter
}

// WithModerator adapts the legacy Moderator contract to policy decisions.
// Not passing a policy engine or legacy moderator means M8 passes through.
func WithModerator(m Moderator) ModerationOption {
	return moderationOptionFunc(func(c *moderationConfig) { c.engine = moderation.NewLegacyEngine(m) })
}

// WithPolicyEngine injects the explicit policy-decision contract. When both
// options are present, the last option wins.
func WithPolicyEngine(engine PolicyEngine) ModerationOption {
	return moderationOptionFunc(func(c *moderationConfig) { c.engine = engine })
}

// WithPolicyAuditTracer injects the shared audit output used by M8. Valid
// decisions are flushed from M8's post-handler phase after c.Next returns.
func WithPolicyAuditTracer(tracer AuditTracer) ModerationOption {
	return moderationOptionFunc(func(c *moderationConfig) { c.auditTracer = tracer })
}

func WithPolicyResolver(resolver policy.Resolver) ModerationOption {
	return moderationOptionFunc(func(c *moderationConfig) { c.resolver = resolver })
}

func WithPolicyDocumentAdapter(adapter policy.DocumentAdapter) ModerationOption {
	return moderationOptionFunc(func(c *moderationConfig) { c.documents = adapter })
}

// Moderation is M8: evaluates the input policy decision and injects an output
// policy adapter into the request context so the invoker can enforce decisions
// through moderation.WrapStream while constructing the response stream.
//
// Failure behavior:
//   - Envelope missing → 500 (defensive, should not happen)
//   - Policy engine unavailable or invalid decision → 503
//   - Deny decision → 400 / content_rejected
//   - Redact decision without a mutation executor → 503 (fail closed)
//
// When no policy engine is injected → c.Next() passes through directly.
func Moderation(opts ...ModerationOption) gin.HandlerFunc {
	cfg := moderationConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}

	if cfg.engine == nil && cfg.resolver == nil {
		// pass-through fast path: doesn't even open a tracer.
		return func(c *gin.Context) { c.Next() }
	}

	if cfg.documents == nil {
		cfg.documents = policy.JSONDocumentAdapter{}
	}

	tracer := otel.GetTracerProvider().Tracer(ScopeName)

	return func(c *gin.Context) {
		ctx, span := tracer.Start(c.Request.Context(), "moderation.check")
		defer span.End()

		c.Request = c.Request.WithContext(ctx)

		audits := make([]policy.AuditRecord, 0, 2)
		defer func() {
			for _, record := range audits {
				metric.Inc(metric.PolicyEnforcementTotal,
					"stage", string(record.Stage), "action", string(record.Action), "result", string(record.Enforcement))

				if cfg.auditTracer != nil {
					cfg.auditTracer.Log(ctx, "policy_decision", record)
				}
			}
		}()

		rc := GetRequestContext(c)
		if rc.Envelope == nil {
			abortWithCode(c, 500, domain.ErrUnknown, domain.ErrCodeInternalError,
				"internal: M3 Envelope did not run before M8")

			return
		}

		subject := policy.Subject{
			AccountID: rc.Identity.AccountID,
			APIKeyID:  rc.Identity.APIKeyID,
		}

		var definition *policy.Definition
		if cfg.resolver != nil {
			var resolveErr error

			definition, resolveErr = cfg.resolver.Resolve(ctx, subject)
			if resolveErr != nil {
				audits = append(audits, policyFailureAudit(nil, policy.StageInput, "binding_unavailable"))

				abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
					"policy binding unavailable")

				return
			}

			if definition != nil {
				if err := definition.Validate(); err != nil {
					audits = append(audits, policyFailureAudit(nil, policy.StageInput, "binding_invalid"))

					abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
						"policy binding is invalid")

					return
				}

				c.Header(HeaderGatewayPolicyID, definition.Ref.ID+"@"+strconv.FormatUint(definition.Ref.Version, 10))
				c.Header(HeaderGatewayPolicyOutputMode, string(definition.OutputMode))
			}
		}

		if cfg.engine == nil {
			if definition == nil || (!definition.InputEnabled && definition.OutputMode == policy.OutputDisabled) {
				c.Next()

				return
			}

			abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
				"policy engine is not configured")

			audits = append(audits, policyFailureAudit(&definition.Ref, policy.StageInput, "engine_not_configured"))

			return
		}

		input := policy.EvaluationInput{
			Stage:   policy.StageInput,
			Subject: subject,
			Model:   rc.Envelope.Model, Modality: rc.Envelope.Modality,
			Content: policy.Content{MediaType: "application/json", Bytes: rc.Envelope.RawBytes},
			Request: rc.Envelope,
		}
		if definition != nil {
			input.Policy = &definition.Ref
		}

		segments, extractErr := cfg.documents.Extract(rc.Envelope.RawBytes, rc.Envelope.SourceProtocol, rc.Envelope.Modality)
		if extractErr == nil {
			input.Segments = segments
		}

		inputEnabled := definition == nil || definition.InputEnabled
		if inputEnabled {
			decision, err := cfg.engine.Evaluate(ctx, input)
			if err != nil {
				audits = append(audits, policyFailureAudit(input.Policy, policy.StageInput, "engine_unavailable"))

				abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
					"policy engine unavailable")

				return
			}

			if err := decision.Validate(); err != nil {
				audits = append(audits, policyFailureAudit(input.Policy, policy.StageInput, "invalid_decision"))

				abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
					"policy engine returned an invalid decision")

				return
			}

			audit := decision.SafeAudit(policy.StageInput)
			recordPolicyDecision(policy.StageInput, decision.Action)

			switch decision.Action {
			case policy.ActionDeny:
				audits = append(audits, audit.WithEnforcement(policy.EnforcementDenied))
				message := "content rejected by policy"
				// Only the legacy adapter preserves its historical client message.
				// New engines may place sensitive detector context in Cause, so it
				// must never become part of the HTTP response.
				if _, legacy := cfg.engine.(*moderation.LegacyEngine); legacy && decision.Cause != nil {
					message = "content rejected: " + decision.Cause.Error()
				}

				abortWithCode(c, 400, domain.ErrInvalid, domain.ErrCodeContentRejected, message)

				return
			case policy.ActionRedact:
				rebuilt, applyErr := cfg.documents.Apply(rc.Envelope.RawBytes, rc.Envelope.SourceProtocol, rc.Envelope.Modality, decision.Mutations)
				if applyErr != nil {
					audits = append(audits, audit.WithEnforcement(policy.EnforcementFailed))

					abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
						"policy mutation could not be applied")

					return
				}

				rc.Envelope.RawBytes = rebuilt
				c.Request.Body = io.NopCloser(bytes.NewReader(rebuilt))
				c.Request.ContentLength = int64(len(rebuilt))
				input.Content.Bytes = rebuilt
				input.Segments, _ = cfg.documents.Extract(rebuilt, rc.Envelope.SourceProtocol, rc.Envelope.Modality)

				audits = append(audits, audit.WithEnforcement(policy.EnforcementApplied))
			case policy.ActionAllow:
				audits = append(audits, audit.WithEnforcement(policy.EnforcementAllowed))
			default:
				abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
					"policy engine returned an unsupported action")

				return
			}
		}

		outputMode := policy.OutputBestEffortStreaming

		maxBufferBytes := policy.DefaultMaxBufferBytes
		if definition != nil {
			outputMode = definition.OutputMode
			maxBufferBytes = definition.MaxBufferBytes
		}

		if outputMode == policy.OutputDisabled {
			c.Next()

			return
		}

		// Reuse the established stream decorator through an explicit-engine
		// adapter; output decisions therefore share the same contract.
		outputModerator := moderation.NewPolicyModerator(cfg.engine, input, func(record policy.AuditRecord) {
			audits = append(audits, record)
			metric.Inc(metric.PolicyDecisionsTotal, "stage", string(record.Stage), "action", string(record.Action))
		}, moderation.WithDocumentAdapter(cfg.documents), moderation.WithOutputMode(outputMode, maxBufferBytes))
		c.Request = c.Request.WithContext(moderation.ContextWithModerator(ctx, outputModerator))

		c.Next()
	}
}

func recordPolicyDecision(stage policy.Stage, action policy.Action) {
	metric.Inc(metric.PolicyDecisionsTotal, "stage", string(stage), "action", string(action))
}

func policyFailureAudit(ref *policy.PolicyRef, stage policy.Stage, reason string) policy.AuditRecord {
	selected := policy.PolicyRef{ID: "gateway/policy-enforcement", Version: 1, Scope: policy.Scope{Kind: policy.ScopeGlobal}}
	if ref != nil && ref.Validate() == nil {
		selected = *ref
	}

	return policy.AuditRecord{
		Stage: stage, Action: policy.ActionDeny, Policy: selected,
		RuleID: "gateway_policy_enforcement", ReasonCode: reason,
		Enforcement: policy.EnforcementFailed,
	}
}
