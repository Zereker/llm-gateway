package middleware

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// =============================================================================
// M8 Moderation 输出审核：装饰器模式接进 translator pipeline
// =============================================================================
//
// **背景**：CheckInput 是 pre-side（请求 body），M8 自己一次完成。CheckOutput 是
// post-side（流式响应 chunks），需要嵌进 M7 的 chunk loop——但 M7 不该直接依赖
// Moderator。所以走"装饰器 + ctx 传递"模式：
//
//   1. M8 装 Moderator 进 ctx（withModerator）
//   2. M7 创建 translator.ResponseHandler 后用 wrapWithModerator 包一层
//   3. 包装后的 handler 在 inner.Feed 之后调 mod.CheckOutput
//   4. 违规 → Feed 返 error → schedule chunk loop break → 不写本 chunk 到客户端
//      → resp.Body.Close 触发上游断流
//
// **不能"召回已发"**：流式场景下 chunk 一旦写到 c.Writer 就到客户端了；CheckOutput
// 只能"检测到违规时切断后续"，不能撤回前序 chunks。这是流式审核固有约束，跟
// 实现无关。

// rcModeratorKey 私有 typed key，跟 RC ctx key 一样的模式（避免外部包 collision）。
type rcModeratorKey struct{}

// withModerator 把 Moderator 注入 ctx。M8 调，M7 间接通过 wrapWithModerator 读出。
//
// mod 为 nil 时 noop（返回原 ctx）；让 caller 不必判 nil。
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

// WrapWithModerator 用 moderatedResponseHandler 包装 inner handler。
//
// ctx 为 nil 或 ctx 里没 moderator → 返回 inner 不动（避免 wrap 开销）。
// 给 M7 + cmd/gateway invoker adapter 用——M8 通过 withModerator 把 Moderator
// 塞进 ctx，调用方在构造 ResponseHandler 后 wrap 一次即可。
func WrapWithModerator(inner translator.ResponseHandler, ctx context.Context) translator.ResponseHandler {
	mod := moderatorFromCtx(ctx)
	if mod == nil {
		return inner
	}
	return &moderatedResponseHandler{inner: inner, mod: mod, ctx: ctx}
}

// wrapWithModerator 旧名（保留 unexported alias，待 PR3 清理）。
//
// 仅 pkg/middleware 内部 + 既有测试用。新代码统一走 WrapWithModerator。
func wrapWithModerator(inner translator.ResponseHandler, ctx context.Context) translator.ResponseHandler {
	return WrapWithModerator(inner, ctx)
}

// moderatedResponseHandler 装饰器：在 translator inner Feed 之后插入 Moderator.CheckOutput。
//
// 检测到违规后置位 violated flag + 返 error；schedule 的 chunk loop 见 error 即 break，
// 本 chunk 的字节**不会**写到 c.Writer（违规字节没流出）。后续 Feed 都直接短路返 err，
// 防止 race condition 下还有 chunk 在 pipe 里。
//
// **CheckOutput 调在 inner.Feed 之后**：让 moderator 看到"客户端会真看到的字节"，
// 而不是上游原始 chunk（translator 可能改过 shape）。
type moderatedResponseHandler struct {
	inner    translator.ResponseHandler
	mod      Moderator
	ctx      context.Context
	violated atomic.Bool // 检测到违规 → 后续 Feed 全部短路
}

// Feed 透传给 inner，再 CheckOutput；违规 → 返 error 让 schedule 中止流。
func (h *moderatedResponseHandler) Feed(chunk []byte) ([]byte, error) {
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
			// 把 out 丢弃（不让违规字节回到 schedule）；返 error 中断 stream
			return nil, fmt.Errorf("moderation: output violated: %w", mErr)
		}
	}
	return out, nil
}

// Flush 透传给 inner，对 final 字节再做一次 CheckOutput。
//
// non-streaming（buffer-then-translate）路径里 Feed 始终返 nil，只 Flush 给 final body；
// 必须在 Flush 也审一次，否则 non-streaming 模式下 moderator 永远拿不到 output。
func (h *moderatedResponseHandler) Flush() ([]byte, *domain.Usage, error) {
	finalOut, usage, err := h.inner.Flush()
	if err != nil {
		return finalOut, usage, err
	}
	if h.violated.Load() {
		// 流式中已检出过；Flush final 字节也别送
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
// schedule chunk loop 把所有 Feed err 都当中止信号；不需要专门识别本 sentinel。
var ErrModerationViolated = errors.New("moderation: output violated; stream aborted")
