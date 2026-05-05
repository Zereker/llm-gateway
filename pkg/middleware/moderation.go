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
// Moderator 为 nil 时 handler 静默 pass-through——开发期或不需审核的部署可省略；
// 接入 OpenAI moderation / 自研内容安全时显式注入实现。
type ModerationDeps struct {
	Moderator Moderator
}

// Moderation 是 M8：对请求 body 做 input 审核 + 把 Moderator 注入 ctx 让 M7
// pipeline 接 output 审核。
//
// **两阶段审核**（input + output）：
//   - **pre-side**：CheckInput(rc.Envelope)；违规直接 abort 400
//   - **post-side**（间接）：M8 通过 ctx 把 Moderator 传给 M7 schedule；M7 创建
//     translator.ResponseHandler 时用 wrapWithModerator 包一层，每 chunk 流出
//     给客户端前调 CheckOutput；违规中断流（unsent chunk 丢弃，已 sent 不能召回）
//
// 失败行为：
//   - Envelope 缺失（M3 没跑） → 直接 pass（防御性，不应发生）
//   - CheckInput 报错 → 400 / ErrInvalid / "content rejected: <err>"
//   - CheckOutput 报错 → schedule chunk loop break（rc.Error 在 M7 侧填）
//
// Moderator 为 nil 时直接 c.Next()。
func Moderation(deps ModerationDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps.Moderator == nil {
			c.Next()
			return
		}

		rc := GetRequestContext(c)
		ctx, end := startSpan(rc.Ctx, "ai-gateway.moderation")
		defer end()
		rc.Ctx = ctx

		// 装配契约：M3 必须先跑。fail-fast 而不是静默放过——silent 跳过审核是
		// 安全 hole（违规请求绕过审核进上游）。
		if rc.Envelope == nil {
			abort(c, 500, domain.ErrUnknown, "internal: M3 Envelope did not run before M8")
			return
		}

		if err := deps.Moderator.CheckInput(rc.Ctx, rc.Envelope); err != nil {
			abort(c, 400, domain.ErrInvalid, "content rejected: "+err.Error())
			return
		}

		// 把 Moderator 装进 ctx，让 M7 schedule 包 ResponseHandler 时拿到
		// （详见 moderation_handler.go 的 wrapWithModerator）
		rc.Ctx = withModerator(rc.Ctx, deps.Moderator)

		c.Next()
	}
}
