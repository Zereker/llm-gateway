package middleware

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// =============================================================================
// M8 Moderation 输出审核：装饰器模式接进 protocol pipeline
// =============================================================================
//
// **背景**：CheckInput 是 pre-side（请求 body），M8 自己一次完成。CheckOutput 是
// post-side（流式响应 chunks），需要嵌进 dispatcher 的 chunk loop——但 dispatcher
// 不该直接依赖 Moderator。所以走"装饰器 + ctx 传递"模式：
//
//   1. M8 装 Moderator 进 ctx（withModerator）
//   2. dispatch_wiring 拿到 handler.NewResponseStream() 后用 WrapStreamWithModerator 包一层
//   3. 包装后的 stream 在 inner.Feed 之后调 mod.CheckOutput
//   4. 违规 → Feed 返 error → invoker.Forward 的 chunk loop break → 不写本 chunk 到客户端
//      → resp.Body.Close 触发上游断流
//
// **不能"召回已发"**：流式场景下 chunk 一旦写到 c.Writer 就到客户端了；CheckOutput
// 只能"检测到违规时切断后续"，不能撤回前序 chunks。这是流式审核固有约束。

// rcModeratorKey 私有 typed key。
type rcModeratorKey struct{}

// withModerator 把 Moderator 注入 ctx。M8 调，dispatch_wiring 间接通过 WrapStreamWithModerator 读出。
func withModerator(ctx context.Context, mod Moderator) context.Context {
	if mod == nil {
		return ctx
	}
	return context.WithValue(ctx, rcModeratorKey{}, mod)
}

// moderatorFromCtx 提取 ctx 里的 Moderator；没注入返 nil。
func moderatorFromCtx(ctx context.Context) Moderator {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(rcModeratorKey{}).(Moderator)
	return v
}

// WrapStreamWithModerator 用 moderatedStream 包装 inner protocol.ResponseStream。
//
// ctx 为 nil 或 ctx 里没 moderator → 返回 inner 不动（避免 wrap 开销）。
func WrapStreamWithModerator(inner protocol.ResponseStream, ctx context.Context) protocol.ResponseStream {
	mod := moderatorFromCtx(ctx)
	if mod == nil {
		return inner
	}
	return &moderatedStream{inner: inner, mod: mod, ctx: ctx}
}

// moderatedStream 装饰器：在 inner Feed 之后插入 Moderator.CheckOutput。
//
// 检测到违规后置位 violated flag + 返 error；invoker.Forward 的 chunk loop 见
// error 即 break，本 chunk 的字节**不会**写到客户端。后续 Feed 都直接短路返 err。
//
// **CheckOutput 调在 inner.Feed 之后**：让 moderator 看到"客户端会真看到的字节"，
// 而不是上游原始 chunk（translator 可能改过 shape）。
type moderatedStream struct {
	inner    protocol.ResponseStream
	mod      Moderator
	ctx      context.Context
	violated atomic.Bool
}

// Feed 透传给 inner，再 CheckOutput；违规 → 返 error 让 forward 中止流。
func (h *moderatedStream) Feed(chunk []byte) ([]byte, error) {
	if h.violated.Load() {
		return nil, ErrModerationViolated
	}
	out, err := h.inner.Feed(chunk)
	if err != nil {
		return out, err
	}
	if len(out) > 0 {
		if mErr := h.mod.CheckOutput(h.ctx, out); mErr != nil {
			h.violated.Store(true)
			return nil, fmt.Errorf("moderation: output violated: %w", mErr)
		}
	}
	return out, nil
}

// Flush 透传给 inner，对 final 字节再做一次 CheckOutput。
//
// non-streaming（buffer-then-translate）路径里 Feed 始终返 nil，只 Flush 给 final body；
// 必须在 Flush 也审一次，否则 non-streaming 模式下 moderator 永远拿不到 output。
func (h *moderatedStream) Flush() ([]byte, *domain.Usage, error) {
	finalOut, usage, err := h.inner.Flush()
	if err != nil {
		return finalOut, usage, err
	}
	if h.violated.Load() {
		return nil, usage, ErrModerationViolated
	}
	if len(finalOut) > 0 {
		if mErr := h.mod.CheckOutput(h.ctx, finalOut); mErr != nil {
			h.violated.Store(true)
			return nil, usage, fmt.Errorf("moderation: output violated (flush): %w", mErr)
		}
	}
	return finalOut, usage, nil
}

// ErrModerationViolated 装饰器内部用：标识"已检出违规，后续 Feed 全部 fail"。
var ErrModerationViolated = errors.New("moderation: output violated; stream aborted")
