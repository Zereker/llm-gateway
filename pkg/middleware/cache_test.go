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

// cacheHarness builds a gin engine chaining [seed RC] -> [ResponseCache] -> [downstream].
// downstream increments calls++ on every call and writes rc.Usage plus a 200 JSON body.
func cacheHarness(store ResponseCacheStore) (*gin.Engine, *int) {
	return cacheHarnessDown(store, func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Usage = &domain.Usage{Input: 10, Output: 5, Total: 15}
		c.Data(http.StatusOK, "application/json", []byte(`{"resp":true}`))
	})
}

// cacheHarnessDown is the same as above but lets downstream be customized (for testing
// poisoning / SSE paths, etc).
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
// second call hits (downstream is not called).
func TestResponseCache_DeterministicHitMiss(t *testing.T) {
	store := newFakeCacheStore()
	e, calls := cacheHarness(store)
	body := `{"model":"m","temperature":0,"messages":[{"role":"user","content":"hi"}]}`

	w1 := postCache(e, body, "")
	if w1.Code != 200 || *calls != 1 || store.sets != 1 {
		t.Fatalf("miss: code=%d calls=%d sets=%d, want 200/1/1", w1.Code, *calls, store.sets)
	}
	if w1.Header().Get(HeaderGatewayCache) == "hit" {
		t.Error("the first call should not be a hit")
	}

	w2 := postCache(e, body, "")
	if w2.Code != 200 || *calls != 1 { // downstream must not be called again
		t.Fatalf("hit: code=%d calls=%d, want 200/1 (a hit must not call downstream)", w2.Code, *calls)
	}
	if w2.Header().Get(HeaderGatewayCache) != "hit" {
		t.Error("the second call should carry X-Gateway-Cache: hit")
	}
	if w2.Body.String() != `{"resp":true}` {
		t.Errorf("hit body = %q, want cached body", w2.Body.String())
	}
}

// Poisoning defense: a 200 with a non-nil rc.Error (stream interrupted / upstream error,
// truncated body) must never be cached.
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

// SSE Content-Type fallback: even if a request is misclassified as non-streaming, a
// text/event-stream response must not be cached.
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

// A malformed stream field (the string "true") is leniently treated as streaming ->
// bypass (even with X-Gateway-Cache: on).
func TestResponseCache_MalformedStreamDetected(t *testing.T) {
	store := newFakeCacheStore()
	e, calls := cacheHarness(store)
	body := `{"model":"m","stream":"true","temperature":0}`
	postCache(e, body, "on")
	postCache(e, body, "on")
	if store.sets != 0 || *calls != 2 {
		t.Errorf("a malformed stream field should be treated as streaming and bypassed, sets=%d calls=%d want 0/2", store.sets, *calls)
	}
}

// Non-deterministic request (no temperature): always bypass, never cached.
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

// X-Gateway-Cache: off bypasses; on forces caching (even when temperature != 0).
func TestResponseCache_HeaderOverrides(t *testing.T) {
	// off: bypass even a deterministic request
	store := newFakeCacheStore()
	e, calls := cacheHarness(store)
	det := `{"model":"m","temperature":0,"messages":[]}`
	postCache(e, det, "off")
	postCache(e, det, "off")
	if *calls != 2 || store.sets != 0 {
		t.Errorf("off: calls=%d sets=%d, want 2/0", *calls, store.sets)
	}

	// on: cache even a non-deterministic request
	store2 := newFakeCacheStore()
	e2, calls2 := cacheHarness(store2)
	nondet := `{"model":"m","temperature":0.9,"messages":[]}`
	postCache(e2, nondet, "on")
	w := postCache(e2, nondet, "on")
	if *calls2 != 1 || w.Header().Get(HeaderGatewayCache) != "hit" {
		t.Errorf("on: calls=%d hit=%q, want 1/hit (forced caching)", *calls2, w.Header().Get(HeaderGatewayCache))
	}
}
