// Package moderation provides content moderation: the Moderator interface + a
// response stream decorator + ctx propagation helpers.
//
// **Architecture position**: extracted from the original pkg/middleware — both
// dispatcher and invoker need to wrap the response stream to moderate output,
// but neither should reverse-depend on middleware; moderation being its own
// package keeps both sides clean.
//
// **Usage shape**:
//
//	M8 middleware:
//	  ctx = moderation.ContextWithModerator(ctx, mod)   // stash into ctx
//	  c.Request = c.Request.WithContext(ctx)
//	  c.Next()
//
//	inside dispatch / invoker (when constructing the response stream):
//	  stream := moderation.WrapStream(handler.NewResponseStream(), ctx)
//	  // the wrapped stream calls mod.CheckOutput on Feed/Flush; a violation → return error to cut off the stream
//
// See docs/architecture/01-request-pipeline.md, the M8 section, for details.
package moderation

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// Moderator is the content moderation port.
//
// **CheckInput**: pre-side, done once against the request body; called
// directly by the M8 middleware.
// **CheckOutput**: post-side, fed chunk by chunk; called by moderation.WrapStream
// after protocol.ResponseStream.Feed/Flush.
type Moderator interface {
	CheckInput(ctx context.Context, env *domain.RequestEnvelope) error
	CheckOutput(ctx context.Context, chunk []byte) error
}

// =============================================================================
// ctx propagation
// =============================================================================

type ctxKey struct{}

// ContextWithModerator injects the Moderator into ctx. Called by M8; read back
// downstream by WrapStream.
// If mod is nil, returns the original ctx unchanged (caller doesn't need to nil-check).
//
// **Naming**: aligned with the standard library's context.WithValue style —
// making the "returns a new ctx" semantics explicit; this avoids confusion with
// pkg/middleware's gin-style "Option" interface (which also commonly uses the
// WithX pattern).
func ContextWithModerator(ctx context.Context, mod Moderator) context.Context {
	if mod == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, mod)
}

// FromContext extracts the Moderator from ctx; returns nil if none was injected.
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
// If ctx is nil or ctx has no moderator, returns inner unchanged (avoiding wrap
// overhead).
//
// **Usage convention**: caller wraps immediately after constructing the stream:
//
//	stream := moderation.WrapStream(handler.NewResponseStream(), ctx)
//	sender.Forward(ctx, w, ep, resp, stream)
func WrapStream(inner protocol.ResponseStream, ctx context.Context) protocol.ResponseStream {
	mod := FromContext(ctx)
	if mod == nil {
		return inner
	}
	return &moderatedStream{inner: inner, mod: mod, ctx: ctx}
}

// moderatedStream is a decorator: it inserts Moderator.CheckOutput after the
// inner Feed call.
//
// When a violation is detected → Feed returns error → invoker.Forward's chunk
// loop breaks → this chunk's bytes **will not** be written to the client.
// Subsequent Feed calls all short-circuit and return err directly.
//
// **CheckOutput is called after inner.Feed**: the moderator sees "the bytes the
// client will actually see", not the raw upstream chunk (the translator may
// have altered its shape).
type moderatedStream struct {
	inner    protocol.ResponseStream
	mod      Moderator
	ctx      context.Context
	violated atomic.Bool
}

// Feed passes through to inner, then CheckOutput; a violation → return error to
// let forward abort the stream.
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

// Flush passes through to inner, then does one more CheckOutput on the final
// bytes.
//
// In the non-streaming (buffer-then-translate) path, Feed always returns nil
// and only Flush produces the final body; so Flush must also be checked once.
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

// ErrViolated is used internally by the decorator: it flags "a violation has
// been detected, all subsequent Feed calls fail".
// invoker.Forward treats every Feed err as an abort signal; there's no need to
// specifically recognize this sentinel.
var ErrViolated = errors.New("moderation: output violated; stream aborted")
