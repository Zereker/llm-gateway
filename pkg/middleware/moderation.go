package middleware

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// Moderator M8 Content Moderation middleware 的依赖接口。
//
// 默认 nil = NoOp；接入审核服务时实现自定义 Moderator。
//
// Implementations MUST be safe for concurrent use。
// CheckOutput 的 chunk 参数：实现不可保留 slice 引用（caller 复用 buffer）。
type Moderator interface {
	CheckInput(c context.Context, env *domain.RequestEnvelope) error
	CheckOutput(c context.Context, chunk []byte) error
}

// ModerationOption 配置 Moderation middleware。
type ModerationOption func(*moderationConfig)

type moderationConfig struct {
	moderator Moderator
}

// WithModerator 注入 Moderator 实现。
//
// 不传 = M8 静默 pass-through（适合开发 / 无审核需求部署）。
func WithModerator(m Moderator) ModerationOption {
	return func(c *moderationConfig) { c.moderator = m }
}

// Moderation 是 M8：对请求 body 做 input 审核 + 把 Moderator 注入 ctx 让 M7
// pipeline 接 output 审核。
//
// 失败行为：
//   - Envelope 缺失 → 500（防御性，不应发生）
//   - CheckInput 报错 → 400 / content_rejected
//
// Moderator 不注入时 → c.Next() 直接放行。
func Moderation(opts ...ModerationOption) gin.HandlerFunc {
	cfg := &moderationConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	return func(c *gin.Context) {
		if cfg.moderator == nil {
			c.Next()
			return
		}

		rc := GetRequestContext(c)
		ctx, end := startSpan(rc.Ctx, "moderation.check")
		defer end()
		rc.Ctx = ctx

		if rc.Envelope == nil {
			abortWithCode(c, 500, domain.ErrUnknown, domain.ErrCodeInternalError,
				"internal: M3 Envelope did not run before M8")
			return
		}

		if err := cfg.moderator.CheckInput(rc.Ctx, rc.Envelope); err != nil {
			abortWithCode(c, 400, domain.ErrInvalid, domain.ErrCodeContentRejected,
				"content rejected: "+err.Error())
			return
		}

		// 把 Moderator 装进 ctx，让 M7 schedule 包 ResponseHandler 时拿到
		rc.Ctx = withModerator(rc.Ctx, cfg.moderator)

		c.Next()
	}
}
