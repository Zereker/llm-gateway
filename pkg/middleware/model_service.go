package middleware

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"

	"github.com/zereker/llm-gateway/pkg/domain"
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
}

// WithModelCatalog injects a ModelCatalog implementation. Required.
func WithModelCatalog(c ModelCatalog) ModelServiceOption {
	return modelServiceOptionFunc(func(cfg *modelServiceConfig) { cfg.catalog = c })
}

// WithSubscriptionChecker injects a SubscriptionChecker implementation. Required.
func WithSubscriptionChecker(s SubscriptionChecker) ModelServiceOption {
	return modelServiceOptionFunc(func(cfg *modelServiceConfig) { cfg.subscriptions = s })
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
			abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
				"model catalog: "+err.Error())
			return
		}
		if ms == nil {
			abortWithCode(c, 404, domain.ErrInvalid, domain.ErrCodeModelNotFound,
				"model not found: "+rc.Envelope.Model)
			return
		}

		subscribed, err := cfg.subscriptions.HasModel(ctx, rc.Identity.AccountID, ms.ID)
		if err != nil {
			abortWithCode(c, 503, domain.ErrTransient, domain.ErrCodeDependencyUnavailable,
				"subscription lookup: "+err.Error())
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
		c.Next()
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
// cmd/gateway/middleware_adapters.go (adaptCatalog / adaptSubscriptions);
// placed at the composition root to avoid a middleware → ratelimit → repo →
// middleware import cycle. middleware no longer imports pkg/repo.
