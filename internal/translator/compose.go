package translator

import (
	"fmt"

	"github.com/zereker/llm-gateway/internal/domain"
)

// Compose chains two translators into one: front (src->pivot) + back (pivot->tgt).
//
// **Purpose**: fallback for a missing direct pair (docs/02 §6a). A direct
// translator is the high-fidelity first choice; when the registry has no
// direct (src, tgt) pair, DefaultLookup tries composing a usable (but possibly
// lossy) translation chain via the OpenAI protocol as pivot, avoiding the need
// to hand-write every protocol pair as they grow O(N×M).
//
//	Request direction:  src body -> front.TranslateRequest -> pivot body -> back.TranslateRequest -> tgt body
//	Response direction: tgt chunks -> back.handler (tgt->pivot) -> pivot body -> front.handler (pivot->src) -> src body
//
// **Lossy warning**: the double hop loses fields the pivot protocol can't
// express (thinking blocks / cache_control / vendor-specific params, etc.).
// Composed pairs are only a fallback; high-traffic combinations should get a
// direct translator written as soon as possible (once registered directly,
// Find hits it and composition automatically steps aside).
//
// **Precondition**: front.Target() == back.Source(), otherwise panic
// (composition only happens inside FindVia, both sides come from the
// registry, so a mismatch is a code bug that should surface at startup).
func Compose(front, back Translator) Translator {
	if front == nil || back == nil {
		panic("translator.Compose: nil translator")
	}
	if front.Target() != back.Source() {
		panic(fmt.Sprintf("translator.Compose: pivot mismatch %s != %s",
			front.Target(), back.Source()))
	}
	return &composed{front: front, back: back}
}

// FindVia first looks for a direct (src, tgt) translator; on a miss it tries
// composing one through the pivot: Find(src, pivot) + Find(pivot, tgt). It
// returns nil if either leg is missing.
//
// **Direct wins**: a hand-written high-fidelity pair always takes precedence
// over composition — adding a direct implementation for a popular combination
// later requires no changes to any caller.
//
// No redundant composition is produced when src == tgt (an identity pair) or
// when src/tgt is itself the pivot: direct Find already covers identity; a
// leg equal to the missing direct pair likewise fails composition and
// returns nil.
func (r *Registry) FindVia(src, tgt, pivot domain.Protocol) Translator {
	if t := r.Find(src, tgt); t != nil {
		return t
	}
	front := r.Find(src, pivot)
	back := r.Find(pivot, tgt)
	if front == nil || back == nil {
		return nil
	}
	return Compose(front, back)
}

// IsComposed reports whether a translator is a pivot-composition product (so
// callers can emit a lossy warning / metric).
func IsComposed(t Translator) bool {
	_, ok := t.(*composed)
	return ok
}

// composed is the implementation behind Compose.
type composed struct {
	front Translator // src -> pivot
	back  Translator // pivot -> tgt
}

func (c *composed) Source() domain.Protocol { return c.front.Source() }
func (c *composed) Target() domain.Protocol { return c.back.Target() }

// TranslateRequest does two hops: src -> pivot -> tgt.
func (c *composed) TranslateRequest(srcBody []byte) ([]byte, error) {
	pivotBody, err := c.front.TranslateRequest(srcBody)
	if err != nil {
		return nil, fmt.Errorf("compose front (%s→%s): %w", c.front.Source(), c.front.Target(), err)
	}
	tgtBody, err := c.back.TranslateRequest(pivotBody)
	if err != nil {
		return nil, fmt.Errorf("compose back (%s→%s): %w", c.back.Source(), c.back.Target(), err)
	}
	return tgtBody, nil
}

// NewResponseHandler composes a pair of handlers per request.
func (c *composed) NewResponseHandler() ResponseHandler {
	return &composedHandler{
		upstream: c.back.NewResponseHandler(),  // tgt response -> pivot format
		client:   c.front.NewResponseHandler(), // pivot format -> src format
	}
}

// composedHandler chains two levels of ResponseHandler.
//
// Upstream chunks go into the upstream level first (tgt->pivot); the pivot
// bytes it emits then go into the client level (pivot->src). Cross-protocol
// handlers are all buffer-then-translate (Feed returns nil, Flush emits
// everything at once), so data typically only flows through the chain on
// Flush; an identity level (streaming pass-through) mixed into the chain also
// works — whatever Feed produces is passed straight to the next level.
type composedHandler struct {
	upstream ResponseHandler // tgt -> pivot
	client   ResponseHandler // pivot -> src
}

func (h *composedHandler) Feed(chunk []byte) ([]byte, error) {
	mid, err := h.upstream.Feed(chunk)
	if err != nil {
		return nil, err
	}
	if len(mid) == 0 {
		return nil, nil
	}
	return h.client.Feed(mid)
}

// Flush drains both levels in order; usage prefers the upstream level (it
// parses the real upstream response; the client level only sees secondhand
// pivot bytes, which may already have lost fields).
func (h *composedHandler) Flush() ([]byte, *domain.Usage, error) {
	midBytes, upUsage, err := h.upstream.Flush()
	if err != nil {
		return nil, nil, err
	}
	var out []byte
	if len(midBytes) > 0 {
		fed, err := h.client.Feed(midBytes)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, fed...)
	}
	tail, clientUsage, err := h.client.Flush()
	if err != nil {
		return nil, nil, err
	}
	out = append(out, tail...)

	usage := upUsage
	if usage == nil {
		usage = clientUsage
	}
	return out, usage, nil
}
