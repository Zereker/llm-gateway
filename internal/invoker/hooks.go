package invoker

import (
	"context"

	"github.com/zereker/llm-gateway/internal/domain"
)

// Hook is the unified observer type; Sender dispatches it based on which
// Observer sub-interface the implementation satisfies.
//
// A single Hook implementation can satisfy multiple Observer interfaces
// (duck typing); Sender buckets it once during New and does no further
// type-assert at runtime.
//
// **Does not mutate bytes, does not abort the main flow**: observers are for
// bystander needs like auditing / async logging / metric reporting. To
// mutate bytes / block violations, use the translator.ResponseHandler
// decorator instead (the M8 moderator shape).
//
// **Synchronous invocation**: Sender does not spawn a goroutine, does not
// pool buffers, and does not recover panics. A slow hook slows down the main
// flow; to go async, spawn `go func() { ... }()` inside OnXxx yourself, and
// copy the chunk out first (the chunk is reused by io.CopyBuffer / the
// internal buf after the callback returns — the same convention as
// database/sql.Rows.Scan / bufio.Scanner.Bytes()).
type Hook any

// =============================================================================
// 5 Observer interfaces: designed in pairs along the "translation boundary"
// =============================================================================
//
// Sender's byte flow:
//
//	   client                                            upstream
//	     │                                                  │
//	     ▼                                                  ▼
//	[ srcBody ] ──TranslateRequest──→ [ upstreamBody ] ──→ HTTP
//	     │                                  │
//	  ClientRequest                  UpstreamRequest
//
//	[ client chunk ] ←──Feed──── [ upstream chunk ] ←── HTTP
//	     │                                │
//	  ClientChunk                  UpstreamChunk
//
// Use the Client* series to audit the raw request/response; use the
// Upstream* series to see what actually gets sent to / received from the
// upstream. The bytes on both sides are identical under the identity
// translator; they differ under a cross-protocol translator (openai_gemini,
// etc.) — observers pick whichever they need.
// =============================================================================

// ClientRequestObserver fires at the very start of Send (before the
// factory / translator lookup).
//
// The body received is the srcBody the caller passed to Send — the client's
// original request body (untranslated). This is the raw bytes the gateway
// received, suitable for compliance auditing / user behavior analysis.
type ClientRequestObserver interface {
	OnClientRequest(ctx context.Context, ep *domain.Endpoint, body []byte)
}

// UpstreamRequestObserver fires after sess.BuildRequest and before httpc.Do.
//
// The body received is the output of translator.TranslateRequest — the
// bytes about to be sent to the upstream. Under the identity protocol it's
// the same as ClientRequest; across protocols it's the translated shape
// (e.g. an OpenAI client → Gemini upstream call gets a Gemini-schema body).
//
// Suitable for debugging / verifying the upstream protocol; compliance
// scenarios generally want the former instead.
type UpstreamRequestObserver interface {
	OnUpstreamRequest(ctx context.Context, ep *domain.Endpoint, body []byte)
}

// UpstreamChunkObserver fires at the entry of translatorWriter.Write (before
// Feed).
//
// What it receives is the raw chunk read directly from the upstream HTTP
// body — not yet translated, not yet moderator-checked.
//
// Suitable for debugging real upstream responses / recording fixtures for
// playback tests.
//
// **The chunk slice is invalid after the callback returns**: to persist it
// you must copy it with `append([]byte(nil), chunk...)`. The underlying buf
// comes from chunkBufPool.
type UpstreamChunkObserver interface {
	OnUpstreamChunk(ctx context.Context, ep *domain.Endpoint, chunk []byte)
}

// ClientChunkObserver fires after translatorWriter inner.Write (in
// buffer-then-translate mode it also fires after the finalOut write at the
// end of Forward).
//
// What it receives is the bytes the client actually received — already
// translated by translator.Feed and passed through decorators (moderator,
// etc.); a chunk blocked by a decorator for violating policy will not
// trigger this callback.
//
// Suitable for user-facing auditing / reconciling against billing data.
//
// **The chunk slice is invalid after the callback returns**: to persist it
// you must copy it.
type ClientChunkObserver interface {
	OnClientChunk(ctx context.Context, ep *domain.Endpoint, chunk []byte)
}

// AttemptCompleteObserver fires when a single Send call finishes (fires on
// both success and failure).
//
// This is a per-attempt event, not per-request — one client request can
// trigger multiple Send calls (retry / fallback), each firing this callback
// once. For per-request events use M10 Tracing's usage.OutboxPublisher.
type AttemptCompleteObserver interface {
	OnAttemptComplete(ctx context.Context, ep *domain.Endpoint, outcome Outcome)
}

// =============================================================================
// Internal bucketing / fan-out
// =============================================================================

// hookSet is the bucketed set of callbacks after Sender's startup-time
// classification; zero type-assert at runtime.
type hookSet struct {
	clientReq   []ClientRequestObserver
	upstreamReq []UpstreamRequestObserver
	upstreamChk []UpstreamChunkObserver
	clientChk   []ClientChunkObserver
	complete    []AttemptCompleteObserver
}

// classifyHooks buckets each Hook by the sub-interfaces it implements; the
// same hook can land in multiple buckets at once.
//
// Order is preserved: the caller's registration order becomes the callback
// order.
func classifyHooks(hooks []Hook) hookSet {
	var hs hookSet
	for _, h := range hooks {
		if o, ok := h.(ClientRequestObserver); ok {
			hs.clientReq = append(hs.clientReq, o)
		}

		if o, ok := h.(UpstreamRequestObserver); ok {
			hs.upstreamReq = append(hs.upstreamReq, o)
		}

		if o, ok := h.(UpstreamChunkObserver); ok {
			hs.upstreamChk = append(hs.upstreamChk, o)
		}

		if o, ok := h.(ClientChunkObserver); ok {
			hs.clientChk = append(hs.clientChk, o)
		}

		if o, ok := h.(AttemptCompleteObserver); ok {
			hs.complete = append(hs.complete, o)
		}
	}

	return hs
}

func (hs hookSet) fireClientRequest(ctx context.Context, ep *domain.Endpoint, body []byte) {
	for _, o := range hs.clientReq {
		o.OnClientRequest(ctx, ep, body)
	}
}

func (hs hookSet) fireUpstreamRequest(ctx context.Context, ep *domain.Endpoint, body []byte) {
	for _, o := range hs.upstreamReq {
		o.OnUpstreamRequest(ctx, ep, body)
	}
}

func (hs hookSet) fireUpstreamChunk(ctx context.Context, ep *domain.Endpoint, chunk []byte) {
	for _, o := range hs.upstreamChk {
		o.OnUpstreamChunk(ctx, ep, chunk)
	}
}

func (hs hookSet) fireClientChunk(ctx context.Context, ep *domain.Endpoint, chunk []byte) {
	for _, o := range hs.clientChk {
		o.OnClientChunk(ctx, ep, chunk)
	}
}

func (hs hookSet) fireComplete(ctx context.Context, ep *domain.Endpoint, out Outcome) {
	for _, o := range hs.complete {
		o.OnAttemptComplete(ctx, ep, out)
	}
}
