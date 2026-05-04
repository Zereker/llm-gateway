package middleware

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// Moderator M8 Content Moderation middleware 的依赖接口。
//
// 默认实现可为 nil（NoOp）；详见 docs/architecture/01 第 6 节 M8。
//
// Implementations MUST be safe for concurrent use（多 gin handler goroutine 同时调用）。
// CheckOutput 的 chunk 参数：实现不可保留 slice 引用（caller 复用 buffer）。
type Moderator interface {
	CheckInput(c context.Context, env *domain.RequestEnvelope) error // 违规返回 error
	CheckOutput(c context.Context, chunk []byte) error               // 流式审核（Session 集成）
}

// ModerationDeps M8 ContentModeration middleware 的依赖。
//
// Moderator 为 nil 时 handler 静默 pass-through——v0.5 默认无审核；
// 接入 OpenAI moderation / 自研内容安全时显式注入实现。
//
// **本 v0.5 实现只跑 CheckInput**——CheckOutput 流式审核需要嵌进 translator
// ResponseHandler 的 Feed，下个迭代再做。
type ModerationDeps struct {
	Moderator Moderator
}

// Moderation 是 M8：对请求 body（rc.Envelope）做内容审核。
//
// 失败行为：
//   - Envelope 缺失（M3 没跑） → 直接 pass（防御性，不应发生）
//   - Moderator 报错 → 400 / ErrInvalid / "content rejected: <err>"
//
// Moderator 为 nil 时直接 c.Next()。
func Moderation(deps ModerationDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps.Moderator == nil {
			c.Next()
			return
		}
		rc := GetRequestContext(c)
		if rc.Envelope == nil {
			c.Next()
			return
		}
		if err := deps.Moderator.CheckInput(rc.Ctx, rc.Envelope); err != nil {
			abort(c, 400, domain.ErrInvalid, "content rejected: "+err.Error())
			return
		}
		c.Next()
	}
}
