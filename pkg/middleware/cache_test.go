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

// cacheHarness 建一个 [seed RC] → [ResponseCache] → [downstream] 的 gin 引擎。
// downstream 每次被调 calls++，写 rc.Usage + 一个 200 JSON body。
func cacheHarness(store ResponseCacheStore) (*gin.Engine, *int) {
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
			rc := GetRequestContext(c)
			rc.Usage = &domain.Usage{Input: 10, Output: 5, Total: 15}
			c.Data(http.StatusOK, "application/json", []byte(`{"resp":true}`))
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

// 确定性请求（temperature=0，非流式）：第一次 miss+存，第二次 hit（不打 downstream）。
func TestResponseCache_DeterministicHitMiss(t *testing.T) {
	store := newFakeCacheStore()
	e, calls := cacheHarness(store)
	body := `{"model":"m","temperature":0,"messages":[{"role":"user","content":"hi"}]}`

	w1 := postCache(e, body, "")
	if w1.Code != 200 || *calls != 1 || store.sets != 1 {
		t.Fatalf("miss: code=%d calls=%d sets=%d, want 200/1/1", w1.Code, *calls, store.sets)
	}
	if w1.Header().Get(HeaderGatewayCache) == "hit" {
		t.Error("首次不该是 hit")
	}

	w2 := postCache(e, body, "")
	if w2.Code != 200 || *calls != 1 { // downstream 不再被调
		t.Fatalf("hit: code=%d calls=%d, want 200/1（命中不打 downstream）", w2.Code, *calls)
	}
	if w2.Header().Get(HeaderGatewayCache) != "hit" {
		t.Error("第二次应带 X-Gateway-Cache: hit")
	}
	if w2.Body.String() != `{"resp":true}` {
		t.Errorf("hit body = %q, want cached body", w2.Body.String())
	}
}

// 非确定请求（无 temperature）：一律 bypass，永不缓存。
func TestResponseCache_NonDeterministicBypass(t *testing.T) {
	store := newFakeCacheStore()
	e, calls := cacheHarness(store)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	postCache(e, body, "")
	postCache(e, body, "")
	if *calls != 2 || store.sets != 0 {
		t.Errorf("non-deterministic: calls=%d sets=%d, want 2/0（每次都打 downstream，不缓存）", *calls, store.sets)
	}
}

// 流式：永不缓存。
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

// X-Gateway-Cache: off 绕过；on 强制缓存（即使 temperature≠0）。
func TestResponseCache_HeaderOverrides(t *testing.T) {
	// off：确定性请求也绕过
	store := newFakeCacheStore()
	e, calls := cacheHarness(store)
	det := `{"model":"m","temperature":0,"messages":[]}`
	postCache(e, det, "off")
	postCache(e, det, "off")
	if *calls != 2 || store.sets != 0 {
		t.Errorf("off: calls=%d sets=%d, want 2/0", *calls, store.sets)
	}

	// on：非确定请求也缓存
	store2 := newFakeCacheStore()
	e2, calls2 := cacheHarness(store2)
	nondet := `{"model":"m","temperature":0.9,"messages":[]}`
	postCache(e2, nondet, "on")
	w := postCache(e2, nondet, "on")
	if *calls2 != 1 || w.Header().Get(HeaderGatewayCache) != "hit" {
		t.Errorf("on: calls=%d hit=%q, want 1/hit（强制缓存）", *calls2, w.Header().Get(HeaderGatewayCache))
	}
}
