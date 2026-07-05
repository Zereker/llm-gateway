package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
)

type fakeCacheStore struct {
	data map[string]CachedResponse
	sets int
}

func newFakeCacheStore() *fakeCacheStore { return &fakeCacheStore{data: map[string]CachedResponse{}} }

func (f *fakeCacheStore) Get(_ context.Context, key string) (CachedResponse, bool) {
	v, ok := f.data[key]
	return v, ok
}
func (f *fakeCacheStore) Set(_ context.Context, key string, r CachedResponse, _ time.Duration) {
	f.data[key] = r
	f.sets++
}

// cacheHarness builds a gin engine chaining [seed RC] → [ResponseCache] → [downstream].
// downstream increments calls++ each time it's invoked, writing rc.Usage + a 200 JSON body.
func cacheHarness(store ResponseCacheStore) (*gin.Engine, *int) {
	return cacheHarnessDown(store, func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Usage = &domain.Usage{Input: 10, Output: 5, Total: 15}
		c.Data(http.StatusOK, "application/json", []byte(`{"resp":true}`))
	})
}

// cacheHarnessDown is the same as above but downstream is customizable (for testing
// poisoning / SSE and other paths).
func cacheHarnessDown(store ResponseCacheStore, down gin.HandlerFunc) (*gin.Engine, *int) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	calls := 0
	e.POST("/v1/chat/completions",
		func(c *gin.Context) {
			rc := &domain.RequestContext{
				Envelope:     &domain.RequestEnvelope{RawBytes: readBody(c), Model: "m", SourceProtocol: domain.ProtoOpenAI},
				ModelService: &domain.ModelService{Model: "m"},
			}
			AttachRequestContext(c, rc)
			c.Next()
		},
		ResponseCache(store, time.Minute),
		func(c *gin.Context) {
			calls++
			down(c)
		},
	)
	return e, &calls
}

func readBody(c *gin.Context) []byte {
	b, _ := c.GetRawData()
	return b
}

func postCache(e *gin.Engine, body, cacheHdr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cacheHdr != "" {
		req.Header.Set(HeaderGatewayCache, cacheHdr)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

// Deterministic request (temperature=0, non-streaming): first call misses and stores,
// second call hits (doesn't reach downstream).
func TestResponseCache_DeterministicHitMiss(t *testing.T) {
	store := newFakeCacheStore()
	e, calls := cacheHarness(store)
	body := `{"model":"m","temperature":0,"messages":[{"role":"user","content":"hi"}]}`

	w1 := postCache(e, body, "")
	if w1.Code != 200 || *calls != 1 || store.sets != 1 {
		t.Fatalf("miss: code=%d calls=%d sets=%d, want 200/1/1", w1.Code, *calls, store.sets)
	}
	if w1.Header().Get(HeaderGatewayCache) == "hit" {
		t.Error("first call should not be a hit")
	}

	w2 := postCache(e, body, "")
	if w2.Code != 200 || *calls != 1 { // downstream is not called again
		t.Fatalf("hit: code=%d calls=%d, want 200/1 (a hit must not call downstream)", w2.Code, *calls)
	}
	if w2.Header().Get(HeaderGatewayCache) != "hit" {
		t.Error("second call should carry X-Gateway-Cache: hit")
	}
	if w2.Body.String() != `{"resp":true}` {
		t.Errorf("hit body = %q, want cached body", w2.Body.String())
	}
}

// Poisoning safeguard: a 200 with rc.Error non-nil (stream broke / upstream errored, body
// truncated) must never be cached.
func TestResponseCache_TruncatedNotCached(t *testing.T) {
	store := newFakeCacheStore()
	e, calls := cacheHarnessDown(store, func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Usage = &domain.Usage{Total: 15}
		rc.Error = &domain.AdapterError{Class: domain.ErrTransient, Code: domain.ErrCodeUpstreamError, Message: "stream reset mid-body"}
		c.Data(http.StatusOK, "application/json", []byte(`{"parti`)) // half a body
	})
	body := `{"model":"m","temperature":0,"messages":[]}`
	postCache(e, body, "")
	postCache(e, body, "")
	if store.sets != 0 {
		t.Errorf("a truncated response (rc.Error!=nil) should not be cached, sets=%d want 0", store.sets)
	}
	if *calls != 2 {
		t.Errorf("since nothing was cached, both calls should hit downstream, calls=%d want 2", *calls)
	}
}

// SSE Content-Type fallback: even if misclassified as non-streaming, a text/event-stream
// response must never be cached.
func TestResponseCache_EventStreamNotCached(t *testing.T) {
	store := newFakeCacheStore()
	e, _ := cacheHarnessDown(store, func(c *gin.Context) {
		c.Data(http.StatusOK, "text/event-stream", []byte("data: hi\n\n"))
	})
	body := `{"model":"m","temperature":0}`
	postCache(e, body, "")
	if store.sets != 0 {
		t.Errorf("a text/event-stream response should not be cached, sets=%d want 0", store.sets)
	}
}

// A malformed stream field (string "true") is leniently detected as streaming → bypass
// (even with X-Gateway-Cache: on).
func TestResponseCache_MalformedStreamDetected(t *testing.T) {
	store := newFakeCacheStore()
	e, calls := cacheHarness(store)
	body := `{"model":"m","stream":"true","temperature":0}`
	postCache(e, body, "on")
	postCache(e, body, "on")
	if store.sets != 0 || *calls != 2 {
		t.Errorf("a malformed stream field should be judged as streaming and bypass, sets=%d calls=%d want 0/2", store.sets, *calls)
	}
}

// Non-deterministic request (no temperature): always bypasses, never cached.
func TestResponseCache_NonDeterministicBypass(t *testing.T) {
	store := newFakeCacheStore()
	e, calls := cacheHarness(store)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	postCache(e, body, "")
	postCache(e, body, "")
	if *calls != 2 || store.sets != 0 {
		t.Errorf("non-deterministic: calls=%d sets=%d, want 2/0 (every call hits downstream, nothing cached)", *calls, store.sets)
	}
}

// Embeddings are inherently deterministic: cached by default even without temperature
// (Modality==ModalityEmbedding override).
func TestResponseCache_EmbeddingsDeterministicByDefault(t *testing.T) {
	store := newFakeCacheStore()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	calls := 0
	e.POST("/v1/embeddings",
		func(c *gin.Context) {
			rc := &domain.RequestContext{
				Envelope: &domain.RequestEnvelope{
					RawBytes: readBody(c), Model: "text-embedding-3-small",
					SourceProtocol: domain.ProtoOpenAI, Modality: domain.ModalityEmbedding,
				},
				ModelService: &domain.ModelService{Model: "text-embedding-3-small"},
			}
			AttachRequestContext(c, rc)
			c.Next()
		},
		ResponseCache(store, time.Minute),
		func(c *gin.Context) {
			calls++
			rc := GetRequestContext(c)
			rc.Usage = &domain.Usage{Input: 4, Total: 4}
			c.Data(http.StatusOK, "application/json", []byte(`{"data":[{"embedding":[0.1,0.2]}]}`))
		},
	)
	// No temperature, no stream -- for chat this would go through non-deterministic
	// bypass, but embeddings should be cached by default.
	body := `{"model":"text-embedding-3-small","input":"hello world"}`
	req := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		return w
	}
	w1 := req()
	if w1.Code != 200 || calls != 1 || store.sets != 1 {
		t.Fatalf("miss: code=%d calls=%d sets=%d, want 200/1/1", w1.Code, calls, store.sets)
	}
	w2 := req()
	if w2.Code != 200 || calls != 1 {
		t.Fatalf("hit: code=%d calls=%d, want 200/1 (a hit must not call downstream)", w2.Code, calls)
	}
	if w2.Header().Get(HeaderGatewayCache) != "hit" {
		t.Error("second embeddings call should be a hit")
	}
}

// cacheKey folds in modality: same protocol/model/body but different modality → different
// key (prevents chat and embeddings from colliding and returning the wrong response across
// modalities).
func TestCacheKey_ModalityNamespaced(t *testing.T) {
	body := []byte(`{"model":"m","input":"x","temperature":0}`)
	kChat := cacheKey(domain.ProtoOpenAI, domain.ModalityChat, "m", body)
	kEmb := cacheKey(domain.ProtoOpenAI, domain.ModalityEmbedding, "m", body)
	if kChat == kEmb {
		t.Errorf("chat and embedding with the same body should not collide on key: %s", kChat)
	}
	// Same modality, same input must be stable (so it can hit).
	if cacheKey(domain.ProtoOpenAI, domain.ModalityEmbedding, "m", body) != kEmb {
		t.Error("same modality + same input should produce a stable key")
	}
}

// Streaming: never cached.
func TestResponseCache_StreamBypass(t *testing.T) {
	store := newFakeCacheStore()
	e, calls := cacheHarness(store)
	body := `{"model":"m","stream":true,"temperature":0,"messages":[]}`
	postCache(e, body, "")
	postCache(e, body, "")
	if *calls != 2 || store.sets != 0 {
		t.Errorf("stream: calls=%d sets=%d, want 2/0", *calls, store.sets)
	}
}

// X-Gateway-Cache: off bypasses; on forces caching (even when temperature≠0).
func TestResponseCache_HeaderOverrides(t *testing.T) {
	// off: bypasses even a deterministic request
	store := newFakeCacheStore()
	e, calls := cacheHarness(store)
	det := `{"model":"m","temperature":0,"messages":[]}`
	postCache(e, det, "off")
	postCache(e, det, "off")
	if *calls != 2 || store.sets != 0 {
		t.Errorf("off: calls=%d sets=%d, want 2/0", *calls, store.sets)
	}

	// on: caches even a non-deterministic request
	store2 := newFakeCacheStore()
	e2, calls2 := cacheHarness(store2)
	nondet := `{"model":"m","temperature":0.9,"messages":[]}`
	postCache(e2, nondet, "on")
	w := postCache(e2, nondet, "on")
	if *calls2 != 1 || w.Header().Get(HeaderGatewayCache) != "hit" {
		t.Errorf("on: calls=%d hit=%q, want 1/hit (forced caching)", *calls2, w.Header().Get(HeaderGatewayCache))
	}
}
