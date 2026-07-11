package invoker

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// chunkBufPool reuses the 4KiB read buffer used for stream forwarding.
//
// Under high-QPS streaming, allocating make([]byte, 4096) per request would
// noticeably increase GC pressure. The pool stores *[]byte to avoid
// sync.Pool interface{} boxing (the pattern recommended by the Go FAQ).
//
// 4KiB: a typical SSE chunk is a few hundred bytes to 1KiB; one 4KiB Read
// usually grabs one or two chunks — a good fit. When going through
// io.CopyBuffer this buf is fed straight to stdlib, zero extra allocation.
var chunkBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 4096)
		return &b
	},
}

// ForwardResult is the return value once Forward completes.
//
// Usage may be nil (translator didn't parse one out / upstream returned
// none). FeedErr is an abort error encountered during streaming (resp.Body
// Read / handler.Feed / Flush failed); non-nil means the stream is also
// interrupted from the client's point of view (bytes already written can't
// be recalled).
//
// TTFTMs: the elapsed time from the start of Forward to the first
// content-bearing client chunk (docs/05 §4). 0 means it wasn't captured
// (buffer-then-translate / empty upstream response / handler produces no
// output during Feed).
type ForwardResult struct {
	Usage   *domain.Usage
	FeedErr error
	TTFTMs  int64
}

// startTimeCtxKey is the context key used to inject the "request start
// time" into Forward for TTFT computation.
//
// If the caller (M7) wants TTFT timed from before Sender.Send (covering the
// whole BuildRequest / translator / client.Do span), it stashes startTime in
// ctx. Otherwise Forward uses its own start moment as the baseline (only
// counting "first-byte latency visible to the client").
type startTimeCtxKey struct{}

// WithRequestStartTime lets the caller inject the "request start time" into
// ctx so Forward can compute TTFT from it.
//
// M7 should call this once before Send: `ctx = invoker.WithRequestStartTime(ctx, time.Now())`.
func WithRequestStartTime(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, startTimeCtxKey{}, t)
}

func requestStartTime(ctx context.Context, fallback time.Time) time.Time {
	if v, ok := ctx.Value(startTimeCtxKey{}).(time.Time); ok && !v.IsZero() {
		return v
	}
	return fallback
}

// Forward streams a successful upstream response forward to ResponseWriter.
//
// **w is the stdlib `http.ResponseWriter`**: gin.ResponseWriter /
// echo.Response / httptest.NewRecorder all satisfy it automatically —
// internal/upstream is not tied to any framework.
//
// Internally it drives chunk flow with io.CopyBuffer, writing to a
// translatorWriter destination — which wraps handler.Feed as an io.Writer:
// each chunk is translated by Feed → written to the client + flushed.
// CopyBuffer uses the buf obtained from chunkBufPool directly as the read
// buffer, zero extra allocation.
//
// **Cannot use io.Copy(w, resp.Body) directly**: every chunk must go through
// the translator (protocol translation / moderator output check / SSE
// reframing / usage parsing), and handler.Flush() must run after EOF —
// io.Copy knows about neither of these.
//
// **Hook fan-out** (see hooks.go for details):
//   - UpstreamChunkObserver: at the entry of translatorWriter.Write, before Feed — the raw upstream chunk
//   - ClientChunkObserver: after inner.Write + the finalOut write at Flush time — the bytes the client actually sees
//
// The chunk slice is only valid during the callback; an observer that needs
// to persist it must copy it itself.
//
// **Failure semantics**: once streaming has started (headers already
// written), no error can roll back the status code. Abort errors are
// written to ForwardResult.FeedErr; the caller writes rc.Error / logs on its
// own — what the client sees is a truncated stream.
func (s *Sender) Forward(
	ctx context.Context,
	w http.ResponseWriter,
	ep *domain.Endpoint,
	resp *http.Response,
	stream protocol.ResponseStream,
) ForwardResult {
	defer func() { _ = resp.Body.Close() }()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flush(w)

	bufPtr := chunkBufPool.Get().(*[]byte)
	defer chunkBufPool.Put(bufPtr)

	startTime := requestStartTime(ctx, time.Now())
	tw := &streamWriter{
		inner:     w,
		stream:    stream,
		ctx:       ctx,
		ep:        ep,
		hooks:     s.hooks,
		startTime: startTime,
	}
	_, feedErr := io.CopyBuffer(tw, resp.Body, *bufPtr)

	finalOut, usage, fErr := stream.Flush()
	if len(finalOut) > 0 {
		// No chunk was output during streaming (buffer-then-translate); treat
		// Flush as the first packet for TTFT purposes.
		if tw.ttftMs == 0 {
			tw.ttftMs = time.Since(startTime).Milliseconds()
		}
		_, _ = w.Write(finalOut)
		flush(w)
		s.hooks.fireClientChunk(ctx, ep, finalOut)
	}

	if feedErr == nil && fErr != nil {
		feedErr = fErr
	}

	return ForwardResult{Usage: usage, FeedErr: feedErr, TTFTMs: tw.ttftMs}
}

// streamWriter feeds an upstream chunk into protocol.ResponseStream.Feed,
// forwards Feed's output to the real ResponseWriter + flushes it, and
// fans out hooks on both sides of Feed.
//
// This wrapping layer lets io.CopyBuffer drive the whole streaming pipeline
// (src=resp.Body, dst=streamWriter).
//
// **Write return-value convention**: returns len(chunk) (the length of the
// original chunk, not Feed's output length) — this is what the io.Copy
// contract requires ("the entire input was consumed"). When Feed errors, it
// returns (0, err) so CopyBuffer stops immediately.
type streamWriter struct {
	inner     http.ResponseWriter
	stream    protocol.ResponseStream
	ctx       context.Context
	ep        *domain.Endpoint
	hooks     hookSet
	startTime time.Time
	ttftMs    int64 // filled once the first non-empty client chunk is written; 0 = not captured
}

func (tw *streamWriter) Write(chunk []byte) (int, error) {
	tw.hooks.fireUpstreamChunk(tw.ctx, tw.ep, chunk)

	out, err := tw.stream.Feed(chunk)
	if err != nil {
		return 0, err
	}
	if len(out) > 0 {
		if _, werr := tw.inner.Write(out); werr != nil {
			return 0, werr
		}
		flush(tw.inner)
		// Record TTFT: elapsed time from startTime to the first non-empty
		// chunk being written out (docs/05 §4).
		if tw.ttftMs == 0 {
			tw.ttftMs = time.Since(tw.startTime).Milliseconds()
		}
		tw.hooks.fireClientChunk(tw.ctx, tw.ep, out)
	}
	return len(chunk), nil
}

// flush calls http.Flusher.Flush; a no-op if w doesn't implement Flusher.
//
// Production http.ResponseWriter implementations (net/http stdlib server /
// gin / echo) all implement Flusher; only test stubs like
// httptest.NewRecorder may not. Degraded semantics: data is sent only once
// the buffer is full / at EOF — correctness is unaffected, only streaming
// is lost.
func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// blockedUpstreamHeaders are response headers that must not be relayed from the
// upstream provider to the client. Keys are in http.Header canonical form.
//
//   - Content-Length: the downstream server recomputes it.
//   - Set-Cookie / Set-Cookie2: never hand an upstream provider's cookies to
//     our client (session fixation / cross-tenant cookie bleed).
//   - hop-by-hop headers (RFC 9110 §7.6.1): meaningless to forward end-to-end.
//   - upstream identity / quota disclosure: which vendor+org backs a model and
//     the gateway's shared upstream rate-limit state are internal details.
var blockedUpstreamHeaders = map[string]struct{}{
	"Content-Length":      {},
	"Set-Cookie":          {},
	"Set-Cookie2":         {},
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
	"Openai-Organization": {},
	"Openai-Project":      {},
	"Openai-Version":      {},
}

// copyHeaders relays upstream response headers to the client, minus the
// blocklist and the x-ratelimit-* family (upstream quota disclosure). The
// gateway surfaces its own state via X-Gateway-* / X-RateLimit-* headers.
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if _, blocked := blockedUpstreamHeaders[k]; blocked {
			continue
		}
		if len(k) >= 12 && strings.EqualFold(k[:12], "X-Ratelimit-") {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
