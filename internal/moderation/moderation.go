// Package moderation implements content moderation: the Moderator interface,
// a response stream decorator, and ctx-passing helpers.
//
// **Architectural placement**: extracted out of the original internal/middleware —
// both dispatcher and invoker need to wrap the response stream to moderate
// output, but neither can depend back on middleware; splitting moderation
// into its own package keeps both sides clean.
//
// **Usage shape**:
//
//	M8 middleware:
//	  ctx = moderation.ContextWithModerator(ctx, mod)   // stash into ctx
//	  c.Request = c.Request.WithContext(ctx)
//	  c.Next()
//
//	inside dispatch / invoker (when constructing the response stream):
//	  stream := moderation.WrapStream(ctx, handler.NewResponseStream())
//	  // the wrapped stream calls mod.CheckOutput on Feed/Flush; a violation → return error to cut off the stream
//
// See the M8 section of docs/architecture/01-request-pipeline.md for details.
package moderation

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// Moderator is the content moderation port.
//
// **CheckInput**: pre-side, run once against the full request body; called
// directly by the M8 middleware.
// **CheckOutput**: post-side, fed chunk by chunk; called by moderation.WrapStream
// after protocol.ResponseStream.Feed/Flush.
type Moderator interface {
	CheckInput(ctx context.Context, env *domain.RequestEnvelope) error
	CheckOutput(ctx context.Context, chunk []byte) error
}

// =============================================================================
// ctx passing
// =============================================================================

type ctxKey struct{}

// ContextWithModerator injects a Moderator into ctx. Called by M8; read back
// downstream by WrapStream. Returns the original ctx when mod is nil (callers
// don't need to nil-check).
//
// **Naming**: aligned with the stdlib context.WithValue convention — making
// the "returns a new ctx" semantics explicit, to avoid confusion with
// internal/middleware's gin-style "Option" interface (which also commonly uses the
// WithX pattern).
func ContextWithModerator(ctx context.Context, mod Moderator) context.Context {
	if mod == nil {
		return ctx
	}

	return context.WithValue(ctx, ctxKey{}, mod)
}

// FromContext extracts the Moderator stored in ctx; returns nil if none was injected.
func FromContext(ctx context.Context) Moderator {
	if ctx == nil {
		return nil
	}

	v, _ := ctx.Value(ctxKey{}).(Moderator)

	return v
}

// =============================================================================
// WrapStream — response stream decorator
// =============================================================================

// WrapStream wraps the inner protocol.ResponseStream with a moderatedStream.
//
// If ctx is nil or has no moderator in it, returns inner untouched (avoids the
// wrapping overhead).
//
// **Usage convention**: the caller wraps immediately after constructing the stream:
//
//	stream := moderation.WrapStream(ctx, handler.NewResponseStream())
//	sender.Forward(ctx, w, ep, resp, stream)
func WrapStream(ctx context.Context, inner protocol.ResponseStream) protocol.ResponseStream {
	mod := FromContext(ctx)
	if mod == nil {
		return inner
	}

	return &moderatedStream{inner: inner, mod: mod, ctx: ctx}
}

// moderatedStream decorator: inserts a Moderator.CheckOutput call right after inner's Feed.
//
// When a violation is detected → Feed returns an error → invoker.Forward's
// chunk loop breaks → this chunk's bytes are **not** written to the client.
// Subsequent Feed calls all short-circuit and return the same err.
//
// **CheckOutput is called after inner.Feed**: the moderator sees "the bytes
// the client will actually see", not the raw upstream chunk (the translator
// may have reshaped it).
//
// **Under streaming, CheckOutput's input is a single SSE frame**: the out
// produced by each Feed call is usually one frame (data: {...}\n\n). This
// means substring/regex-based guards can only catch patterns that fall within
// a single frame under streaming; patterns split across frames (tokens each
// forming their own frame, with framing bytes in between) can't be scanned —
// even buffering across chunks can't reassemble them. A hard guarantee
// requires the non-streaming path — Flush submits the entire body for
// checking at once. This is an inherent constraint of streaming content
// moderation (bytes already sent can't be recalled), not something this
// decorator alone can fix; see the DenylistGuard type doc for details.
type moderatedStream struct {
	inner    protocol.ResponseStream
	mod      Moderator
	ctx      context.Context
	violated atomic.Bool
}

// Feed passes through to inner, then calls CheckOutput; a violation → returns
// an error so forward aborts the stream.
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

// Flush passes through to inner, then runs CheckOutput once more on the final bytes.
//
// In the non-streaming (buffer-then-translate) path, Feed always returns nil
// and only Flush delivers the final body; it must be checked here too.
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

// ErrViolated is used internally by the decorator to mark "a violation has
// been detected; all subsequent Feed calls fail". invoker.Forward treats any
// Feed err as an abort signal, so there's no need to specifically recognize
// this sentinel.
var ErrViolated = errors.New("moderation: output violated; stream aborted")
