package middleware

import (
	"bytes"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	cacheport "github.com/zereker/llm-gateway/internal/cache"
	"github.com/zereker/llm-gateway/internal/embed"
	"github.com/zereker/llm-gateway/internal/metric"
)

// SemanticCache is an advanced form of response caching: it hits based on the **vector
// similarity** of the request prompt, rather than exact byte matching — paraphrased
// requests with different wording but the same meaning can share a cache entry.
//
// Flow (hit/write-back/usage logic shares helpers with the exact cache, inheriting the H1
// poisoning safeguard + M4 streaming fallback):
//  1. Extract the prompt (messages content + system) → convert to a vector via Embedder
//  2. Look for a prior entry with cosine similarity ≥ threshold within the (protocol|model)
//     namespace → return it on a hit
//  3. On a miss: tee the response, and if it's a clean 200, store it along with the vector
//
// **Same safety defaults as the exact cache**: non-streaming + temperature=0;
// X-Gateway-Cache off/on override this. If Embed fails → don't cache, pass through
// normally (an embedder error never aborts the request).
// **Note**: a semantic lookup requires vectorizing the prompt first, and embedder.Embed is
// called **synchronously** before c.Next() — a slow embedder adds up to one embed timeout
// of latency to eligible requests (see OpenAIEmbedder's client.Timeout). In production the
// embedder endpoint must be low-latency and highly available, otherwise the semantic cache
// ends up hurting p99 instead of helping; use X-Gateway-Cache=off or temperature≠0 to work
// around this.
// If either store or embedder is nil, the whole middleware is a no-op.
func SemanticCache(store SemanticCacheStore, embedder embed.Embedder, threshold float64, ttl time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if store == nil || embedder == nil {
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
			c.Next()
			return
		}
		if mode != "on" && !deterministic {
			metric.Inc(metric.ResponseCacheTotal, "result", "bypass")
			c.Next()
			return
		}
		prompt := extractPrompt(rc.Envelope.RawBytes)
		if prompt == "" {
			c.Next()
			return
		}

		ctx := c.Request.Context()
		vec, err := embedder.Embed(ctx, prompt)
		if err != nil {
			// embedder hiccup → don't cache, pass through (don't block the request).
			metric.Inc(metric.ResponseCacheTotal, "result", "embed_error")
			c.Next()
			return
		}
		// Namespace is tenant-scoped: a semantic (paraphrase) hit returns the
		// stored completion verbatim, so entries must never cross accounts.
		ns := rc.Identity.AccountID + "|" + rc.Envelope.SourceProtocol.String() + "|" + rc.ModelService.Model

		if cached, ok := store.Lookup(ctx, ns, vec, threshold); ok {
			metric.Inc(metric.ResponseCacheTotal, "result", "semantic_hit")
			writeCacheHit(c, rc, cached)
			return
		}

		metric.Inc(metric.ResponseCacheTotal, "result", "semantic_miss")
		tw := &teeWriter{ResponseWriter: c.Writer, buf: &bytes.Buffer{}}
		c.Writer = tw
		c.Next()

		if resp, ok := cacheableResponse(tw, rc); ok {
			store.Store(ctx, ns, vec, resp, ttl)
			metric.Inc(metric.ResponseCacheTotal, "result", "semantic_store")
		}
	}
}

// SemanticCacheStore is the semantic-cache storage port.
type SemanticCacheStore = cacheport.SemanticStore

// extractPrompt extracts the text to embed from the request body, covering three client
// entry points:
//   - OpenAI / Anthropic ChatCompletion: each message's content + the top-level system field
//   - OpenAI Responses: the top-level input (string or array) + instructions
//
// When content/input is an array (multimodal), its JSON string is used as-is — good enough
// for embedding. The Responses body reaches here via RawBytes (the client's original text
// before translation); failing to cover it would leave the semantic cache silently
// non-functional for Responses clients.
func extractPrompt(body []byte) string {
	var sb strings.Builder
	gjson.GetBytes(body, "messages.#.content").ForEach(func(_, v gjson.Result) bool {
		sb.WriteString(v.String())
		sb.WriteByte('\n')
		return true
	})
	if s := gjson.GetBytes(body, "system"); s.Exists() {
		sb.WriteString(s.String())
		sb.WriteByte('\n')
	}
	// Responses protocol: input may be a string or an array of items.
	if in := gjson.GetBytes(body, "input"); in.Exists() {
		if in.IsArray() {
			in.ForEach(func(_, v gjson.Result) bool {
				sb.WriteString(v.String())
				sb.WriteByte('\n')
				return true
			})
		} else {
			sb.WriteString(in.String())
			sb.WriteByte('\n')
		}
	}
	if ins := gjson.GetBytes(body, "instructions"); ins.Exists() {
		sb.WriteString(ins.String())
	}
	return strings.TrimSpace(sb.String())
}
