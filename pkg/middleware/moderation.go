package middleware

import (
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/moderation"
)

// Moderator 是 moderation.Moderator 的别名，保留旧 import 路径。
// 新代码请直接用 pkg/moderation.Moderator。
type Moderator = moderation.Moderator

// ModerationOption 配置 Moderation middleware（otelgin v0.68.0 同款 interface-Option）。
type ModerationOption interface {
	apply(*moderationConfig)
}

type moderationOptionFunc func(*moderationConfig)

func (f moderationOptionFunc) apply(c *moderationConfig) { f(c) }

type moderationConfig struct {
	moderator Moderator
}

// WithModerator 注入 Moderator 实现。不传 = M8 静默 pass-through。
func WithModerator(m Moderator) ModerationOption {
	return moderationOptionFunc(func(c *moderationConfig) { c.moderator = m })
}

// Moderation 是 M8：对请求 body 做 input 审核 + 把 Moderator 注入 ctx 让
// invoker 在构造 response stream 时通过 moderation.WrapStream 接 output 审核。
//
// 失败行为：
//   - Envelope 缺失 → 500（防御性，不应发生）
//   - CheckInput 报错 → 400 / content_rejected
//
// Moderator 不注入时 → c.Next() 直接放行。
func Moderation(opts ...ModerationOption) gin.HandlerFunc {
	cfg := moderationConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.moderator == nil {
		// pass-through 快路径：连 tracer 都不开。
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

		// 把 Moderator 装进 ctx，让 invoker 包 ResponseStream 时拿到。
		c.Request = c.Request.WithContext(moderation.WithModerator(ctx, cfg.moderator))

		c.Next()
	}
}
