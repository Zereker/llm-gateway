package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	cacheport "github.com/zereker/llm-gateway/internal/cache"
	"github.com/zereker/llm-gateway/internal/requeststate"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/metric"
)

// ResponseCache is the response-cache middleware — on a hit it returns the cached response
// directly, skipping M7 Schedule (no upstream call), saving cost and latency. It sits after
// M6 Limit and before M7 Schedule: a hit still counts against RPM (Limit has already
// deducted it), but incurs zero upstream cost.
//
// **By default only deterministic requests are cached**: non-streaming + temperature=0.
// Non-deterministic requests (temperature≠0 / omitted) would get stale results back from
// the cache and behave unpredictably, so they're skipped by default; the client can force
// caching with X-Gateway-Cache: on (at their own risk), or bypass entirely with off.
// Streaming is **never** cached (v1).
//
// **embeddings exception**: embedding requests have no sampling parameters, so the same
// input always produces the same vector — inherently deterministic
// (Modality==ModalityEmbedding) → cacheable by default, no temperature=0 required. This
// pays off big in high-hit-rate scenarios (RAG repeatedly embedding the same batch of text).
//
// **key** = SHA256(accountID | sourceProtocol | modality | canonical model | request body).
// The account is folded in so the cache is **tenant-scoped**: a prompt typically contains
// tenant data, and the cached response echoes it (system_fingerprint / ids / the completion
// itself), so sharing entries across accounts would leak one tenant's content to another.
//
// **usage**: on a hit, the usage stored in the cache is passed through (Source=cache), M10
// still emits the usage event as normal, and M6 still deducts TPM afterward — downstream can
// decide to bill zero cost based on source=cache.
//
// When store == nil (not configured), the whole middleware is a no-op and doesn't affect the chain.
//
// **Known trade-offs (opt-in feature, off by default; deployers should be aware when enabling it)**:
//   - The key uses the canonical model name (not the specific endpoint/upstream version) —
//     the cache is checked **before** M7 routing picks an endpoint, so no endpoint is known
//     yet to fold into the key. This assumes a model_service maps to **stable** upstream
//     output; don't enable caching for a model_service backed by multiple upstream versions
//     under the same name (gpt-4o-2024-05 vs -11).
//   - A hit still emits a usage event (source=cache) + counts TPM (a soft counter): a cache
//     hit still counts as having "delivered N tokens," and downstream decides whether to
//     bill it as zero cost based on source=cache.
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
		// Embeddings have no sampling parameters (temperature/top_p don't apply) — the same
		// input always yields the same vector, inherently deterministic, so it should be
		// cached by default (no need for temperature=0 / X-Gateway-Cache: on). High hit rate,
		// big payoff (embedding is often recomputed repeatedly over the same batch of text).
		if rc.Envelope.Modality == domain.ModalityEmbedding {
			deterministic = true
		}
		if mode != "on" && !deterministic {
			metric.Inc(metric.ResponseCacheTotal, "result", "bypass")
			c.Next() // by default only deterministic requests are cached
			return
		}

		ctx := c.Request.Context()
		key := cacheKey(rc.Identity.AccountID, rc.Envelope.SourceProtocol, rc.Envelope.Modality, rc.ModelService.Model, rc.Envelope.RawBytes)

		// Hit: write the cached response + pass through usage, abort to skip M7 (M6-post/M10
		// still run on the onion's return leg).
		if cached, ok := store.Get(ctx, key); ok {
			metric.Inc(metric.ResponseCacheTotal, "result", "hit")
			writeCacheHit(c, rc, cached)
			return
		}

		// Miss: tee the response, write it back to the cache on success.
		metric.Inc(metric.ResponseCacheTotal, "result", "miss")
		tw := &teeWriter{ResponseWriter: c.Writer, buf: &bytes.Buffer{}}
		c.Writer = tw
		c.Next()

		if resp, ok := cacheableResponse(tw, rc); ok {
			store.Set(ctx, key, resp, ttl)
			metric.Inc(metric.ResponseCacheTotal, "result", "store")
		}
	}
}

// writeCacheHit writes the cached response to the client + passes through usage
// (Source=cache) + aborts to skip M7. Shared by exact-cache and semantic-cache hits.
func writeCacheHit(c *gin.Context, rc *requeststate.State, cached CachedResponse) {
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
}

// cacheableResponse determines whether the teed response is cacheable, returning a
// CachedResponse if so. Only **clean, complete, non-streaming** 200s are cached:
//   - rc.Error != nil: the stream broke / upstream errored after a 200, the body may be
//     truncated, and caching it would poison subsequent requests.
//   - text/event-stream: a fallback for when analyzeBody misclassifies a request as
//     non-streaming (SSE must never be cached).
//
// Shared by exact-cache and semantic-cache write-back — both inherit the H1 poisoning
// safeguard and the M4 streaming fallback.
func cacheableResponse(tw *teeWriter, rc *requeststate.State) (CachedResponse, bool) {
	ct := tw.Header().Get("Content-Type")
	if tw.Status() == 200 && tw.buf.Len() > 0 && rc.Error == nil && !isEventStream(ct) {
		return CachedResponse{StatusCode: 200, ContentType: ct, Body: tw.buf.Bytes(), Usage: rc.Usage}, true
	}
	return CachedResponse{}, false
}

// ResponseCacheStore is the response-cache storage port.
type ResponseCacheStore = cacheport.Store

// CachedResponse is retained as an alias for source compatibility. The cache
// capability owns the value; middleware only consumes it.
type CachedResponse = cacheport.CachedResponse

// cacheKey is the hex of SHA256(accountID | protocol | modality | model | body).
//
// **accountID must be folded in**: the cache is tenant-scoped so one account's cached
// prompt/response can't be served to another (the request body and completion routinely
// carry tenant data).
//
// **modality must be folded in**: chat and embeddings are both ProtoOpenAI, and the exact
// cache shares one Redis store/prefix; without modality a byte-identical body (e.g.
// {"model":"m","input":"x","temperature":0}) sent to /v1/embeddings and
// /v1/chat/completions would collide → a cross-modality request would get the wrong
// response. Folding in modality keeps the two keyspaces cleanly separated.
func cacheKey(accountID string, proto domain.Protocol, modality domain.Modality, model string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(accountID))
	h.Write([]byte{0})
	h.Write([]byte(proto.String()))
	h.Write([]byte{0})
	h.Write([]byte(modality.String()))
	h.Write([]byte{0})
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write(body)
	return "resp:" + hex.EncodeToString(h.Sum(nil))
}

// analyzeBody parses (stream, deterministic) from the request body.
//
// **Uses gjson with a lenient stream check**: consistent with Envelope's schema-less
// extraction, and gjson.Bool() coerces malformed values like "true" / 1 to true too —
// avoiding the case where encoding/json's strict parsing fails and a request that will
// actually stream gets misclassified as non-streaming, causing the whole SSE stream to get
// buffered into the cache.
//
// deterministic = temperature is explicitly 0 (most vendors default temperature to 1 when
// omitted, treated as non-deterministic).
func analyzeBody(body []byte) (stream, deterministic bool) {
	// One pass over the body for both fields instead of two GetBytes scans.
	res := gjson.GetManyBytes(body, "stream", "temperature")
	stream = res[0].Bool()
	t := res[1]
	deterministic = t.Exists() && t.Num == 0
	return stream, deterministic
}

// isEventStream reports whether Content-Type is SSE (a fallback safeguard for cache write-back).
func isEventStream(ct string) bool {
	return strings.Contains(strings.ToLower(ct), "text/event-stream")
}

// teeWriter wraps gin.ResponseWriter, also copying the written body into buf (used for
// cache write-back).
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
