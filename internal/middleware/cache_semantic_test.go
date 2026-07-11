package middleware

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/embed"
	"github.com/zereker/llm-gateway/internal/requeststate"
)

// keywordEmbedder maps a prompt to a fixed vector by keyword — prompts in the same bucket
// get the same vector (cosine=1), and buckets are orthogonal to each other (cosine=0). Used
// to deterministically test semantic hits/misses.
type keywordEmbedder struct{ fail bool }

func (e keywordEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if e.fail {
		return nil, context.DeadlineExceeded
	}
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "weather"):
		return []float32{1, 0, 0}, nil
	case strings.Contains(t, "code"):
		return []float32{0, 1, 0}, nil
	default:
		return []float32{0, 0, 1}, nil
	}
}

// memSemanticStore is an in-memory SemanticCacheStore that uses real cosine similarity.
type memSemanticStore struct {
	entries map[string][]struct {
		vec  []float32
		resp CachedResponse
	}
	stores int
}

func newMemSemanticStore() *memSemanticStore {
	return &memSemanticStore{entries: map[string][]struct {
		vec  []float32
		resp CachedResponse
	}{}}
}

func (s *memSemanticStore) Lookup(_ context.Context, ns string, vec []float32, threshold float64) (CachedResponse, bool) {
	var best float64
	var bestResp CachedResponse
	found := false
	for _, e := range s.entries[ns] {
		if sim := embed.Cosine(vec, e.vec); sim >= threshold && sim > best {
			best, bestResp, found = sim, e.resp, true
		}
	}
	return bestResp, found
}

func (s *memSemanticStore) Store(_ context.Context, ns string, vec []float32, resp CachedResponse, _ time.Duration) {
	s.entries[ns] = append(s.entries[ns], struct {
		vec  []float32
		resp CachedResponse
	}{vec, resp})
	s.stores++
}

func semanticHarness(store SemanticCacheStore, emb embed.Embedder) (*gin.Engine, *int) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	calls := 0
	e.POST("/v1/chat/completions",
		func(c *gin.Context) {
			rc := &requeststate.State{
				Envelope:     &domain.RequestEnvelope{RawBytes: readBody(c), Model: "m", SourceProtocol: domain.ProtoOpenAI},
				ModelService: &domain.ModelService{Model: "m"},
			}
			AttachRequestContext(c, rc)
			c.Next()
		},
		SemanticCache(store, emb, 0.95, time.Minute),
		func(c *gin.Context) {
			calls++
			GetRequestContext(c).Usage = &domain.Usage{Total: 9}
			c.Data(http.StatusOK, "application/json", []byte(`{"resp":true}`))
		},
	)
	return e, &calls
}

func chatBody(content string) string {
	return `{"model":"m","temperature":0,"messages":[{"role":"user","content":"` + content + `"}]}`
}

// Semantic hit: a second request with different wording but the same meaning (same
// keyword bucket) hits the first request's cache entry.
func TestSemanticCache_ParaphraseHit(t *testing.T) {
	store := newMemSemanticStore()
	e, calls := semanticHarness(store, keywordEmbedder{})

	// First call: miss + store
	w1 := postCache(e, chatBody("what is the weather today"), "")
	if w1.Code != 200 || *calls != 1 || store.stores != 1 {
		t.Fatalf("miss: code=%d calls=%d stores=%d, want 200/1/1", w1.Code, *calls, store.stores)
	}
	// Different wording, same bucket (weather) → semantic hit, doesn't reach downstream
	w2 := postCache(e, chatBody("how's the weather looking right now"), "")
	if *calls != 1 || w2.Header().Get(HeaderGatewayCache) != "hit" {
		t.Fatalf("paraphrase should be a semantic hit: calls=%d hit=%q", *calls, w2.Header().Get(HeaderGatewayCache))
	}
	if w2.Body.String() != `{"resp":true}` {
		t.Errorf("hit body = %q", w2.Body.String())
	}
}

// Semantic miss: requests with different meaning (different buckets) don't hit.
func TestSemanticCache_DifferentTopicMiss(t *testing.T) {
	store := newMemSemanticStore()
	e, calls := semanticHarness(store, keywordEmbedder{})

	postCache(e, chatBody("what is the weather"), "")    // weather bucket
	postCache(e, chatBody("write some code please"), "") // code bucket, orthogonal → miss
	if *calls != 2 || store.stores != 2 {
		t.Errorf("different meanings should each miss: calls=%d stores=%d, want 2/2", *calls, store.stores)
	}
}

// embedder hiccup → don't cache, pass through (don't block the request).
func TestSemanticCache_EmbedErrorBypass(t *testing.T) {
	store := newMemSemanticStore()
	e, calls := semanticHarness(store, keywordEmbedder{fail: true})
	postCache(e, chatBody("anything"), "")
	if *calls != 1 || store.stores != 0 {
		t.Errorf("embed failure should pass through without caching: calls=%d stores=%d, want 1/0", *calls, store.stores)
	}
}

// Non-deterministic request (no temperature) bypasses by default.
func TestSemanticCache_NonDeterministicBypass(t *testing.T) {
	store := newMemSemanticStore()
	e, calls := semanticHarness(store, keywordEmbedder{})
	body := `{"model":"m","messages":[{"role":"user","content":"weather"}]}`
	postCache(e, body, "")
	postCache(e, body, "")
	if *calls != 2 || store.stores != 0 {
		t.Errorf("non-deterministic should bypass: calls=%d stores=%d, want 2/0", *calls, store.stores)
	}
}

func TestExtractPrompt(t *testing.T) {
	got := extractPrompt([]byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"}],"system":"be nice"}`))
	if !strings.Contains(got, "hello") || !strings.Contains(got, "hi") || !strings.Contains(got, "be nice") {
		t.Errorf("extractPrompt = %q", got)
	}
}

// Responses client bodies use input + instructions (no messages) -- the semantic cache
// must not silently stop working for them.
func TestExtractPrompt_Responses(t *testing.T) {
	got := extractPrompt([]byte(`{"model":"gpt-4o","input":"summarize this","instructions":"be terse"}`))
	if !strings.Contains(got, "summarize this") || !strings.Contains(got, "be terse") {
		t.Errorf("Responses string input: extractPrompt = %q", got)
	}
	arr := extractPrompt([]byte(`{"input":["turn one","turn two"],"instructions":"tone"}`))
	if !strings.Contains(arr, "turn one") || !strings.Contains(arr, "turn two") || !strings.Contains(arr, "tone") {
		t.Errorf("Responses array input: extractPrompt = %q", arr)
	}
}
