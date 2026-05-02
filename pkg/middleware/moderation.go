package middleware

import (
	"context"

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
