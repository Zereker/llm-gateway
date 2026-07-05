package middleware

import (
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/moderation"
)

// Moderator is an alias for moderation.Moderator, kept for the old import path.
// New code should use pkg/moderation.Moderator directly.
type Moderator = moderation.Moderator

// ModerationOption configures the Moderation middleware (same interface-Option pattern as otelgin v0.68.0).
type ModerationOption interface {
	apply(*moderationConfig)
}

type moderationOptionFunc func(*moderationConfig)

func (f moderationOptionFunc) apply(c *moderationConfig) { f(c) }

type moderationConfig struct {
	moderator Moderator
}

// WithModerator injects a Moderator implementation. Not passing one means M8
// silently passes through.
func WithModerator(m Moderator) ModerationOption {
	return moderationOptionFunc(func(c *moderationConfig) { c.moderator = m })
}

// Moderation is M8: performs input moderation on the request body + injects
// the Moderator into ctx so the invoker can hook into output moderation via
// moderation.WrapStream when constructing the response stream.
//
// Failure behavior:
//   - Envelope missing → 500 (defensive, should not happen)
//   - CheckInput errors → 400 / content_rejected
//
// When no Moderator is injected → c.Next() passes through directly.
func Moderation(opts ...ModerationOption) gin.HandlerFunc {
	cfg := moderationConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.moderator == nil {
		// pass-through fast path: doesn't even open a tracer.
		return func(c *gin.Context) { c.Next() }
	}
	tracer := otel.GetTracerProvider().Tracer(ScopeName)

	return func(c *gin.Context) {
		ctx, span := tracer.Start(c.Request.Context(), "moderation.check")
		defer span.End()
		c.Request = c.Request.WithContext(ctx)

		rc := GetRequestContext(c)
		if rc.Envelope == nil {
			abortWithCode(c, 500, domain.ErrUnknown, domain.ErrCodeInternalError,
				"internal: M3 Envelope did not run before M8")
			return
		}

		if err := cfg.moderator.CheckInput(ctx, rc.Envelope); err != nil {
			abortWithCode(c, 400, domain.ErrInvalid, domain.ErrCodeContentRejected,
				"content rejected: "+err.Error())
			return
		}

		// Stash the Moderator in ctx so the invoker can retrieve it when
		// wrapping the ResponseStream.
		c.Request = c.Request.WithContext(moderation.ContextWithModerator(ctx, cfg.moderator))

		c.Next()
	}
}
