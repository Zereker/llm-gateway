// Package moderation 定义 M8 Content Moderation middleware 的依赖。
//
// 默认实现可为 nil（NoOp）；详见 docs/architecture/01 第 6 节 M8。
package moderation

import (
	"context"

	"github.com/zereker-labs/ai-gateway/pkg/ctx"
)

// Moderator 内容审核接口。
type Moderator interface {
	CheckInput(c context.Context, env *ctx.RequestEnvelope) error // 违规返回 error
	CheckOutput(c context.Context, chunk []byte) error             // 流式审核（Session 集成）
}
