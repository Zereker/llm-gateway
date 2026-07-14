package middleware

import (
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

	if cfg.engine == nil {
		// pass-through fast path: doesn't even open a tracer.
		return func(c *gin.Context) { c.Next() }
	}

	tracer := otel.GetTracerProvider().Tracer(ScopeName)

	return func(c *gin.Context) {
		ctx, span := tracer.Start(c.Request.Context(), "moderation.check")
		defer span.End()

		c.Request = c.Request.WithContext(ctx)

		audits := make([]policy.AuditRecord, 0, 2)
		defer func() {
			if cfg.auditTracer == nil {
				return
			}

			for _, record := range audits {
				cfg.auditTracer.Log(ctx, "policy_decision", record)
			}
		}()

		rc := GetRequestContext(c)
		if rc.Envelope == nil {
			abortWithCode(c, 500, domain.ErrUnknown, domain.ErrCodeInternalError,
				"internal: M3 Envelope did not run before M8")

			return
		}

		input := policy.EvaluationInput{
			Stage: policy.StageInput,
			Subject: policy.Subject{
				AccountID: rc.Identity.AccountID,
				APIKeyID:  rc.Identity.APIKeyID,
			},
			Model: rc.Envelope.Model, Modality: rc.Envelope.Modality,
			Content: policy.Content{MediaType: "application/json", Bytes: rc.Envelope.RawBytes},
			Request: rc.Envelope,
		}

		decision, err := cfg.engine.Evaluate(ctx, input)
		if err != nil {
			abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
				"policy engine unavailable")

			return
		}

		if err := decision.Validate(); err != nil {
			abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
				"policy engine returned an invalid decision")

			return
		}

		audits = append(audits, decision.SafeAudit(policy.StageInput))
		recordPolicyDecision(policy.StageInput, decision.Action)

		switch decision.Action {
		case policy.ActionDeny:
			message := "content rejected by policy"
			if decision.Cause != nil {
				// Compatibility only: legacy Moderator errors were historically
				// returned to clients. New engines should never set Cause.
				message = "content rejected: " + decision.Cause.Error()
			}

			abortWithCode(c, 400, domain.ErrInvalid, domain.ErrCodeContentRejected,
				message)

			return
		case policy.ActionRedact:
			abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
				policy.ErrRedactionUnsupported.Error())

			return
		case policy.ActionAllow:
		default:
			abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
				"policy engine returned an unsupported action")

			return
		}

		// Reuse the established stream decorator through an explicit-engine
		// adapter; output decisions therefore share the same contract.
		outputModerator := moderation.NewPolicyModerator(cfg.engine, input, func(record policy.AuditRecord) {
			audits = append(audits, record)
			metric.Inc(metric.PolicyDecisionsTotal, "stage", string(record.Stage), "action", string(record.Action))
		})
		c.Request = c.Request.WithContext(moderation.ContextWithModerator(ctx, outputModerator))

		c.Next()
	}
}

func recordPolicyDecision(stage policy.Stage, action policy.Action) {
	metric.Inc(metric.PolicyDecisionsTotal, "stage", string(stage), "action", string(action))
}
