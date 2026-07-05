package middleware

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/embed"
)

// keywordEmbedder 把 prompt 按关键词映射到一个固定向量——同桶的 prompt 向量相同
// （cosine=1），跨桶正交（cosine=0）。用来确定性地测语义命中/未命中。
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

// memSemanticStore 内存版 SemanticCacheStore，用真 cosine 做相似度。
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
			rc := &domain.RequestContext{
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

// 语义命中：措辞不同但同语义(同关键词桶)的第二个请求命中第一个的缓存。
func TestSemanticCache_ParaphraseHit(t *testing.T) {
	store := newMemSemanticStore()
	e, calls := semanticHarness(store, keywordEmbedder{})

	// 首次：miss + store
	w1 := postCache(e, chatBody("what is the weather today"), "")
	if w1.Code != 200 || *calls != 1 || store.stores != 1 {
		t.Fatalf("miss: code=%d calls=%d stores=%d, want 200/1/1", w1.Code, *calls, store.stores)
	}
	// 措辞不同、同桶(weather) → 语义命中，不打 downstream
	w2 := postCache(e, chatBody("how's the weather looking right now"), "")
	if *calls != 1 || w2.Header().Get(HeaderGatewayCache) != "hit" {
		t.Fatalf("paraphrase 应语义命中: calls=%d hit=%q", *calls, w2.Header().Get(HeaderGatewayCache))
	}
	if w2.Body.String() != `{"resp":true}` {
		t.Errorf("命中 body = %q", w2.Body.String())
	}
}

// 语义未命中：不同语义(不同桶)的请求不命中。
func TestSemanticCache_DifferentTopicMiss(t *testing.T) {
	store := newMemSemanticStore()
	e, calls := semanticHarness(store, keywordEmbedder{})

	postCache(e, chatBody("what is the weather"), "") // weather 桶
	postCache(e, chatBody("write some code please"), "") // code 桶,正交 → miss
	if *calls != 2 || store.stores != 2 {
		t.Errorf("不同语义应各自 miss: calls=%d stores=%d, want 2/2", *calls, store.stores)
	}
}

// embedder 抖动 → 不缓存、放行（不阻塞请求）。
func TestSemanticCache_EmbedErrorBypass(t *testing.T) {
	store := newMemSemanticStore()
	e, calls := semanticHarness(store, keywordEmbedder{fail: true})
	postCache(e, chatBody("anything"), "")
	if *calls != 1 || store.stores != 0 {
		t.Errorf("embed 失败应放行不缓存: calls=%d stores=%d, want 1/0", *calls, store.stores)
	}
}

// 非确定请求（无 temperature）默认 bypass。
func TestSemanticCache_NonDeterministicBypass(t *testing.T) {
	store := newMemSemanticStore()
	e, calls := semanticHarness(store, keywordEmbedder{})
	body := `{"model":"m","messages":[{"role":"user","content":"weather"}]}`
	postCache(e, body, "")
	postCache(e, body, "")
	if *calls != 2 || store.stores != 0 {
		t.Errorf("非确定应 bypass: calls=%d stores=%d, want 2/0", *calls, store.stores)
	}
}

func TestExtractPrompt(t *testing.T) {
	got := extractPrompt([]byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"}],"system":"be nice"}`))
	if !strings.Contains(got, "hello") || !strings.Contains(got, "hi") || !strings.Contains(got, "be nice") {
		t.Errorf("extractPrompt = %q", got)
	}
}
