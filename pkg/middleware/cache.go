package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
)

// ResponseCache is the response-caching middleware — a hit returns the cached
// response directly, skipping M7 scheduling (no upstream call), saving cost and
// latency. Placed after M6 Limit and before M7 Schedule: a hit still counts
// against RPM (Limit already deducted before the cache), but incurs zero
// upstream cost.
//
// **By default only caches deterministic requests**: non-streaming + temperature=0.
// Non-deterministic requests (temperature≠0 / unset) would get stale results from
// the cache and behave strangely, so they're skipped by default; clients can force
// caching with X-Gateway-Cache: on (at their own risk), or bypass entirely with off.
// Streaming is **never** cached (v1).
//
// **key** = SHA256(sourceProtocol | canonical model | request body). Same protocol +
// same model + same body → same response bytes. Shared across accounts (the response
// is the model's output, unrelated to the account), giving a higher hit rate.
//
// **usage**: on a hit, the cached usage is passed through (Source=cache), M10 still
// emits the usage event as normal, and M6 still deducts TPM afterward as normal —
// downstream can bill zero cost based on source=cache.
//
// When store == nil (not configured), the whole middleware is a no-op and does not
// affect the chain.
//
// **Known trade-offs (opt-in feature, off by default; deployers should be aware
// when enabling)**:
//   - The key uses the canonical model name (not the specific endpoint/upstream
//     version) — the cache hits **before** M7 routing, at which point no endpoint
//     has been selected yet, so the key can't be scoped per endpoint. This assumes
//     a model_service maps to a **stable** upstream output; don't enable caching
//     for a model_service that fans out to multiple upstream versions under the
//     same name (gpt-4o-2024-05 vs -11).
//   - Shared across accounts (the response is the model's output, unrelated to
//     the account): higher hit rate, but the system_fingerprint/id in the cached
//     response will cross tenants. Enable only if that's acceptable.
//   - A hit still emits a usage event (source=cache) + counts TPM (soft counter):
//     a cache hit counts as having "delivered N tokens"; downstream decides whether
//     to bill zero cost based on source=cache.
func ResponseCache(store ResponseCacheStore, ttl time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if store == nil {
			c.Next()
			return
		}
		rc := GetRequestContext(c)
		mode := strings.ToLower(strings.TrimSpace(c.GetHeader(HeaderGatewayCache)))
		if mode == "off" || rc.Envelope == nil || rc.ModelService == nil {
			c.Next()
			return
		}

		stream, deterministic := analyzeBody(rc.Envelope.RawBytes)
		if stream {
			c.Next() // streaming is never cached
			return
		}
		if mode != "on" && !deterministic {
			metric.Inc(metric.ResponseCacheTotal, "result", "bypass")
			c.Next() // only deterministic requests are cached by default
			return
		}

		ctx := c.Request.Context()
		key := cacheKey(rc.Envelope.SourceProtocol, rc.ModelService.Model, rc.Envelope.RawBytes)

		// Hit: write the cached response + pass through usage, abort to skip M7
		// (M6-post/M10 still run on the onion's return trip).
		if cached, ok := store.Get(ctx, key); ok {
			metric.Inc(metric.ResponseCacheTotal, "result", "hit")
			ct := cached.ContentType
			if ct == "" {
				ct = "application/json; charset=utf-8"
			}
			c.Header(HeaderGatewayCache, "hit")
			c.Data(cached.StatusCode, ct, cached.Body)
			if cached.Usage != nil {
				u := *cached.Usage
				u.Source = domain.UsageSourceCache
				rc.Usage = &u
			}
			c.Abort()
			return
		}

		// Miss: tee the response, and write it back to the cache on success.
		metric.Inc(metric.ResponseCacheTotal, "result", "miss")
		tw := &teeWriter{ResponseWriter: c.Writer, buf: &bytes.Buffer{}}
		c.Writer = tw
		c.Next()

		// Only cache a **clean, complete, non-streaming** 200:
		//   - rc.Error != nil: stream interrupted after 200 / upstream error —
		//     the body may be truncated, which would poison the cache for all
		//     future identical requests (the 200 header has already been
		//     forwarded, and tw.buf holds a half body).
		//   - text/event-stream: a fallback for when analyzeBody misses a
		//     streaming request (never cache SSE).
		ct := tw.Header().Get("Content-Type")
		if tw.Status() == 200 && tw.buf.Len() > 0 && rc.Error == nil && !isEventStream(ct) {
			store.Set(ctx, key, CachedResponse{
				StatusCode:  200,
				ContentType: ct,
				Body:        tw.buf.Bytes(),
				Usage:       rc.Usage,
			}, ttl)
			metric.Inc(metric.ResponseCacheTotal, "result", "store")
		}
	}
}

// ResponseCacheStore is the response-cache storage port (see the cmd wiring
// point for the Redis implementation).
type ResponseCacheStore interface {
	Get(ctx context.Context, key string) (CachedResponse, bool)
	Set(ctx context.Context, key string, resp CachedResponse, ttl time.Duration)
}

// CachedResponse is one complete cached non-streaming response.
type CachedResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
	Usage       *domain.Usage
}

// cacheKey is the hex of SHA256(protocol | model | body).
func cacheKey(proto domain.Protocol, model string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(proto.String()))
	h.Write([]byte{0})
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write(body)
	return "resp:" + hex.EncodeToString(h.Sum(nil))
}

// analyzeBody parses (stream, deterministic) from the request body.
//
// **Uses gjson with a lenient stream check**: schema-less extraction consistent
// with Envelope, and gjson.Bool() coerces malformed values like "true" / 1 to
// true too — this avoids a strict encoding/json parse failure mis-classifying an
// actually-streaming request as non-streaming and then buffering the whole SSE
// stream into the cache.
//
// deterministic = temperature explicitly equals 0 (most vendors default an
// unset temperature to 1, treated as non-deterministic).
func analyzeBody(body []byte) (stream, deterministic bool) {
	stream = gjson.GetBytes(body, "stream").Bool()
	t := gjson.GetBytes(body, "temperature")
	deterministic = t.Exists() && t.Num == 0
	return stream, deterministic
}

// isEventStream reports whether Content-Type is SSE (a fallback safeguard for
// writing back to the cache).
func isEventStream(ct string) bool {
	return strings.Contains(strings.ToLower(ct), "text/event-stream")
}

// teeWriter wraps gin.ResponseWriter, copying the written body into buf as
// well (used for writing back to the cache).
type teeWriter struct {
	gin.ResponseWriter
	buf *bytes.Buffer
}

func (w *teeWriter) Write(b []byte) (int, error) {
	w.buf.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *teeWriter) WriteString(s string) (int, error) {
	w.buf.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}
