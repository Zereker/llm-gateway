// Package moderation 内容审核：Moderator 接口 + 响应流装饰器 + ctx 传递 helper。
//
// **架构定位**：从原 pkg/middleware 抽出来——dispatcher / invoker 都需要 wrap
// 响应流做 output 审核，但不能反向依赖 middleware；moderation 自己成包后两侧
// 都干净。
//
// **使用形态**：
//
//	M8 middleware:
//	  ctx = moderation.WithModerator(ctx, mod)   // 装进 ctx
//	  c.Request = c.Request.WithContext(ctx)
//	  c.Next()
//
//	dispatch / invoker 内（构造 response stream 时）:
//	  stream := moderation.WrapStream(handler.NewResponseStream(), ctx)
//	  // 包装后的 stream 在 Feed/Flush 时调 mod.CheckOutput；违规 → return error 截流
//
// 详见 docs/architecture/01-request-pipeline.md M8 段。
package moderation

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// Moderator 内容审核 port。
//
// **CheckInput**：pre-side，对请求 body 一次完成；M8 middleware 直接调。
// **CheckOutput**：post-side，逐 chunk 喂；moderation.WrapStream 在
// protocol.ResponseStream.Feed/Flush 之后调。
type Moderator interface {
	CheckInput(ctx context.Context, env *domain.RequestEnvelope) error
	CheckOutput(ctx context.Context, chunk []byte) error
}

// =============================================================================
// ctx 传递
// =============================================================================

type ctxKey struct{}

// WithModerator 把 Moderator 注入 ctx。M8 调；下游 WrapStream 读出。
// mod 为 nil 时返回原 ctx（caller 无须判 nil）。
func WithModerator(ctx context.Context, mod Moderator) context.Context {
	if mod == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, mod)
}

// FromCtx 提取 ctx 里的 Moderator；没注入返 nil。
func FromCtx(ctx context.Context) Moderator {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(ctxKey{}).(Moderator)
	return v
}

// =============================================================================
// WrapStream — 响应流装饰器
// =============================================================================

// WrapStream 用 moderatedStream 包装 inner protocol.ResponseStream。
//
// ctx 为 nil 或 ctx 里没 moderator → 返回 inner 不动（避免 wrap 开销）。
//
// **使用约定**：caller 在构造 stream 后立即 wrap：
//
//	stream := moderation.WrapStream(handler.NewResponseStream(), ctx)
//	sender.Forward(ctx, w, ep, resp, stream)
func WrapStream(inner protocol.ResponseStream, ctx context.Context) protocol.ResponseStream {
	mod := FromCtx(ctx)
	if mod == nil {
		return inner
	}
	return &moderatedStream{inner: inner, mod: mod, ctx: ctx}
}

// moderatedStream 装饰器：在 inner Feed 之后插入 Moderator.CheckOutput。
//
// 检测到违规 → Feed 返 error → invoker.Forward 的 chunk loop break →
// 本 chunk 字节**不会**写到客户端。后续 Feed 都直接短路返 err。
//
// **CheckOutput 调在 inner.Feed 之后**：moderator 看到"客户端会真看到的字节"，
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
		return nil, ErrViolated
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
// non-streaming（buffer-then-translate）路径里 Feed 始终返 nil，只 Flush 给
// final body；必须在 Flush 也审一次。
func (h *moderatedStream) Flush() ([]byte, *domain.Usage, error) {
	finalOut, usage, err := h.inner.Flush()
	if err != nil {
		return finalOut, usage, err
	}
	if h.violated.Load() {
		return nil, usage, ErrViolated
	}
	if len(finalOut) > 0 {
		if mErr := h.mod.CheckOutput(h.ctx, finalOut); mErr != nil {
			h.violated.Store(true)
			return nil, usage, fmt.Errorf("moderation: output violated (flush): %w", mErr)
		}
	}
	return finalOut, usage, nil
}

// ErrViolated 装饰器内部用：标识"已检出违规，后续 Feed 全部 fail"。
// invoker.Forward 把所有 Feed err 都当中止信号；不需要专门识别本 sentinel。
var ErrViolated = errors.New("moderation: output violated; stream aborted")
