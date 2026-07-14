package middleware

import (
	"context"
	"log/slog"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/metric"
	"github.com/zereker/llm-gateway/internal/routingpolicy"
)

// ModelCatalog is used by M5: looks up the global catalog by model string.
//
// This interface is a middleware-owned contract; the repo implementation
// layer maps SQL rows to domain.ModelService.
type ModelCatalog interface {
	GetByModel(c context.Context, model string) (*domain.ModelService, error)
}

// SubscriptionChecker is used by M5: determines whether an account is
// subscribed to a given model_service.
type SubscriptionChecker interface {
	HasModel(c context.Context, accountID string, modelServiceID int64) (bool, error)
}

// ModelServiceOption configures the ModelService middleware (same interface-Option pattern as otelgin v0.68.0).
type ModelServiceOption interface {
	apply(*modelServiceConfig)
}

type modelServiceOptionFunc func(*modelServiceConfig)

func (f modelServiceOptionFunc) apply(c *modelServiceConfig) { f(c) }

type modelServiceConfig struct {
	catalog       ModelCatalog
	subscriptions SubscriptionChecker
	resolver      VirtualModelResolver
}

// VirtualModelResolver resolves names absent from the concrete model catalog.
type VirtualModelResolver interface {
	Resolve(context.Context, routingpolicy.Input) (routingpolicy.Resolution, error)
}

// WithModelCatalog injects a ModelCatalog implementation. Required.
func WithModelCatalog(c ModelCatalog) ModelServiceOption {
	return modelServiceOptionFunc(func(cfg *modelServiceConfig) { cfg.catalog = c })
}

// WithSubscriptionChecker injects a SubscriptionChecker implementation. Required.
func WithSubscriptionChecker(s SubscriptionChecker) ModelServiceOption {
	return modelServiceOptionFunc(func(cfg *modelServiceConfig) { cfg.subscriptions = s })
}

func WithVirtualModelResolver(r VirtualModelResolver) ModelServiceOption {
	return modelServiceOptionFunc(func(cfg *modelServiceConfig) { cfg.resolver = r })
}

// ModelService is M5: rc.Envelope.Model → catalog → verify subscription → rc.ModelService.
//
// Also parses X-Gateway-Fallback-Models (docs/03 §5): writes the primary +
// already-validated fallback models into rc.ModelChain for M7 to consume
// directly. A fallback that doesn't exist or isn't subscribed is silently
// dropped, without affecting the primary request.
//
// Failure behavior (docs/01 §7):
//   - rc.Envelope nil (M3 didn't run) → 500
//   - catalog SQL error → 503 / dependency_unavailable
//   - catalog can't find primary → 404 / model_not_found
//   - account not subscribed to primary → 403 / model_not_subscribed
//   - fallback resolution fails → silently dropped, not blocking (only primary must succeed)
func ModelService(opts ...ModelServiceOption) gin.HandlerFunc {
	cfg := modelServiceConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}

	if cfg.catalog == nil {
		panic("middleware.ModelService: WithModelCatalog required")
	}

	if cfg.subscriptions == nil {
		panic("middleware.ModelService: WithSubscriptionChecker required")
	}

	tracer := otel.GetTracerProvider().Tracer(ScopeName)

	return func(c *gin.Context) {
		ctx, span := tracer.Start(c.Request.Context(), "catalog.resolve")
		defer span.End()

		c.Request = c.Request.WithContext(ctx)

		rc := GetRequestContext(c)
		if rc.Envelope == nil {
			abortWithCode(c, 500, domain.ErrUnknown, domain.ErrCodeInternalError,
				"internal: M3 Envelope did not run before M5")

			return
		}

		ms, err := cfg.catalog.GetByModel(ctx, rc.Envelope.Model)
		if err != nil {
			// Fail-closed 503; the driver error (DB/Redis internals) goes to
			// logs only, never the client body (docs/01 §7).
			slog.ErrorContext(ctx, "m5: model catalog lookup failed", "err", err)
			abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
				"model catalog unavailable")

			return
		}

		if ms == nil && cfg.resolver != nil {
			resolution, resolveErr := cfg.resolver.Resolve(ctx, routingpolicy.Input{
				RequestedModel: rc.Envelope.Model,
				AccountID:      rc.Identity.AccountID,
				Region:         c.GetHeader(HeaderGatewayRegion),
				Modality:       rc.Envelope.Modality,
				Group:          rc.Identity.Group,
				DecisionKey:    rc.RequestID,
			})
			rc.ModelRoutingDecision = &resolution.Decision
			recordRoutingDecision(&resolution.Decision)

			if resolveErr != nil {
				slog.ErrorContext(ctx, "m5: virtual model resolution failed", "err", resolveErr)
				abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
					"routing policy unavailable")

				return
			}

			if resolution.Decision.Reason == domain.RoutingReasonNoPolicy {
				abortWithCode(c, 404, domain.ErrInvalid, domain.ErrCodeVirtualModelPolicyNotFound,
					"virtual model policy not found: "+rc.Envelope.Model)

				return
			}

			if len(resolution.Chain) == 0 {
				abortWithCode(c, 403, domain.ErrPermanent, domain.ErrCodeNoEligibleCandidate,
					"no eligible model candidate")

				return
			}

			rc.ModelService = resolution.Chain[0]
			rc.ModelChain = resolution.Chain

			c.Next()

			return
		}

		if ms == nil {
			abortWithCode(c, 404, domain.ErrInvalid, domain.ErrCodeModelNotFound,
				"model not found: "+rc.Envelope.Model)

			return
		}

		subscribed, err := cfg.subscriptions.HasModel(ctx, rc.Identity.AccountID, ms.ID)
		if err != nil {
			slog.ErrorContext(ctx, "m5: subscription lookup failed", "err", err)
			abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
				"subscription lookup unavailable")

			return
		}

		if !subscribed {
			abortWithCode(c, 403, domain.ErrPermanent, domain.ErrCodeModelNotSubscribed,
				"model not subscribed: "+rc.Envelope.Model)

			return
		}

		rc.ModelService = ms
		rc.ModelChain = resolveModelChain(ctx, cfg, ms, rc.Identity.AccountID,
			parseFallbackModels(c, rc.Envelope.Model))
		rc.ModelRoutingDecision = concreteRoutingDecision(rc.Envelope.Model, rc.ModelChain)
		recordRoutingDecision(rc.ModelRoutingDecision)
		c.Next()
	}
}

func recordRoutingDecision(decision *domain.ModelRoutingDecision) {
	if decision == nil {
		return
	}

	scope := "none"
	if decision.Policy != nil {
		scope = string(decision.Policy.Scope.Kind)
	}

	metric.Inc(metric.RoutingDecisionsTotal,
		"outcome", string(decision.Outcome),
		"reason", string(decision.Reason),
		"scope_kind", scope,
	)
}

func concreteRoutingDecision(requested string, chain []*domain.ModelService) *domain.ModelRoutingDecision {
	candidates := make([]domain.RoutingCandidateDecision, 0, len(chain))
	for i, model := range chain {
		source := domain.RoutingCandidateRequested

		reason := domain.RoutingReasonConcreteModel
		if i > 0 {
			source = domain.RoutingCandidateLegacyHeader
			reason = domain.RoutingReasonLegacyFallbackAccepted
		}

		candidates = append(candidates, domain.RoutingCandidateDecision{
			ModelServiceID: model.ID,
			Model:          model.Model,
			Source:         source,
			Eligible:       true,
			Reason:         reason,
			Order:          i,
		})
	}

	return &domain.ModelRoutingDecision{
		RequestedModel: requested,
		Outcome:        domain.RoutingOutcomeResolved,
		Reason:         domain.RoutingReasonConcreteModel,
		Candidates:     candidates,
	}
}

// resolveModelChain assembles primary + already-validated fallbacks into rc.ModelChain.
//
// **Any** validation failure on the fallback path (catalog miss / subscription
// denied / dependency failure) simply skips that fallback silently — the
// primary already succeeded, so a fallback resolution failure shouldn't drag
// down the main request.
func resolveModelChain(
	ctx context.Context,
	cfg modelServiceConfig,
	primary *domain.ModelService,
	accountID string,
	fallbackModels []string,
) []*domain.ModelService {
	chain := make([]*domain.ModelService, 0, 1+len(fallbackModels))
	chain = append(chain, primary)

	if len(fallbackModels) == 0 {
		return chain
	}

	seen := map[string]struct{}{primary.Model: {}}
	for _, m := range fallbackModels {
		if _, dup := seen[m]; dup {
			continue
		}

		seen[m] = struct{}{}

		ms, err := cfg.catalog.GetByModel(ctx, m)
		if err != nil || ms == nil {
			continue
		}

		subscribed, err := cfg.subscriptions.HasModel(ctx, accountID, ms.ID)
		if err != nil || !subscribed {
			continue
		}

		chain = append(chain, ms)
	}

	return chain
}

// parseFallbackModels reads the X-Gateway-Fallback-Models header (comma-separated,
// dedup while preserving order); docs/03 §5: dedup while preserving order, ignore
// empty models, cap count at MaxFallbackModels.
// The primary itself is excluded from the result — the chain must not contain
// an entry with the same name as primary.
func parseFallbackModels(c *gin.Context, primary string) []string {
	hdr := c.GetHeader(HeaderGatewayFallbackModels)
	if hdr == "" {
		return nil
	}

	seen := make(map[string]struct{}, MaxFallbackModels)

	out := make([]string, 0, MaxFallbackModels)
	for _, m := range strings.Split(hdr, ",") {
		m = strings.TrimSpace(m)
		if m == "" || m == primary {
			continue
		}

		if _, dup := seen[m]; dup {
			continue
		}

		seen[m] = struct{}{}

		out = append(out, m)
		if len(out) >= MaxFallbackModels {
			break
		}
	}

	return out
}

// The old AdaptRepoCatalog / AdaptRepoSubscriptions have moved to
// internal/app/gateway/adapters.go (adaptCatalog / adaptSubscriptions);
// placed at the composition root to avoid a middleware → ratelimit → repo →
// middleware import cycle. middleware no longer imports internal/repo.
