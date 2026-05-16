package middleware

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/ratelimit"
	"github.com/zereker/llm-gateway/pkg/repo"
)

// =============================================================================
// helpers
// =============================================================================

func u32(v uint32) *uint32 { return &v }
func i64(v int64) *int64   { return &v }

// makePolicy 构造一个 quota_policies 行的 rule_json，注入 stubQPProvider。
func makePolicy(id int64, rule map[string]any) *repo.QuotaPolicy {
	js, _ := json.Marshal(rule)
	return &repo.QuotaPolicy{ID: id, Name: "p", RuleJSON: js, Enabled: true}
}

// attachM6Inputs 上游契约：M5 之后的状态（Identity / Envelope / ModelService）。
func attachM6Inputs(model string, accountPolicy, apikeyPolicy *int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Identity = domain.UserIdentity{
			AccountID:            "acc1",
			SubAccountID:         "sub1",
			APIKeyID:             "ak1",
			Group:                "default",
			AccountQuotaPolicyID: accountPolicy,
			APIKeyQuotaPolicyID:  apikeyPolicy,
		}
		rc.Envelope = &domain.RequestEnvelope{
			SourceProtocol: domain.ProtoOpenAI,
			Modality:       domain.ModalityChat,
			Model:          model,
			RawBytes:       []byte(`{"model":"` + model + `","messages":[]}`),
		}
		rc.ModelService = &repo.ModelService{ID: 1, Model: model}
		c.Next()
	}
}

// =============================================================================
// M6 Limit 单元测试
// =============================================================================

func TestLimit_500_M3orM5Missing(t *testing.T) {
	r := newGinTest(TraceContext(), Recover(),
		Limit(LimitDeps{Store: newStubStoreAllowAll(), Policies: ratelimit.NewPolicyCache(&stubQPProvider{}, time.Minute)}),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 500 {
		t.Fatalf("status=%d, want=500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "M3/M5 did not run") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestLimit_NoPolicy_PassThrough(t *testing.T) {
	store := newStubStoreAllowAll()
	pc := ratelimit.NewPolicyCache(&stubQPProvider{}, time.Minute)
	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", nil, nil),
		Limit(LimitDeps{Store: store, Policies: pc}),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if store.reserveCalls.Load() != 0 {
		t.Errorf("ReserveBatch called %d times, want=0 (no policy)", store.reserveCalls.Load())
	}
}

func TestLimit_AccountLayerOnly_Reserves(t *testing.T) {
	pol := makePolicy(1, map[string]any{
		"default": map[string]any{"rpm": 60, "tpm": 100000},
	})
	qp := &stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)
	store := newStubStoreAllowAll()

	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), nil),
		Limit(LimitDeps{Store: store, Policies: pc}),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if store.reserveCalls.Load() != 1 {
		t.Fatalf("ReserveBatch calls=%d, want=1", store.reserveCalls.Load())
	}
	got := store.reserveCaptured[0]
	if len(got) != 2 {
		t.Fatalf("buckets=%d, want=2 (RPM + TPM)", len(got))
	}
	// 第一个 RPM, 第二个 TPM
	if !strings.HasSuffix(got[0].Key, ":rpm") {
		t.Errorf("bucket[0].Key=%q, want ends in :rpm", got[0].Key)
	}
	if !strings.HasSuffix(got[1].Key, ":tpm") {
		t.Errorf("bucket[1].Key=%q, want ends in :tpm", got[1].Key)
	}
	if got[0].Limit != 60 {
		t.Errorf("rpm limit=%d, want=60", got[0].Limit)
	}
}

func TestLimit_TwoLayer_AdditiveAndPerModel(t *testing.T) {
	// account 层：default RPM=60
	accPol := makePolicy(1, map[string]any{
		"default": map[string]any{"rpm": 60},
	})
	// apikey 层：default RPM=30 + per_model[gpt-4o] RPM=10 (additive)
	akPol := makePolicy(2, map[string]any{
		"default":   map[string]any{"rpm": 30},
		"per_model": map[string]any{"gpt-4o": map[string]any{"rpm": 10}},
	})
	qp := &stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: accPol, 2: akPol}}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)
	store := newStubStoreAllowAll()

	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), i64(2)),
		Limit(LimitDeps{Store: store, Policies: pc}),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(store.reserveCaptured[0]) != 3 {
		t.Fatalf("buckets=%d, want=3 (acc default + ak default + ak per-model)",
			len(store.reserveCaptured[0]))
	}
}

func TestLimit_Violated_429_WithRetryAfterAndHeaders(t *testing.T) {
	pol := makePolicy(1, map[string]any{
		"default": map[string]any{"rpm": 1},
	})
	qp := &stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)

	store := &stubStore{
		reserveAllowed: false,
		reserveViol: &ratelimit.BucketViolation{
			Key:        "rl:quota:account:acc1:*:rpm",
			Limit:      1,
			Current:    1,
			RetryAfter: 30 * time.Second,
		},
	}

	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), nil),
		Limit(LimitDeps{Store: store, Policies: pc}),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 429 {
		t.Fatalf("status=%d, want=429", w.Code)
	}
	if w.Header().Get("Retry-After") != "30" {
		t.Errorf("Retry-After=%q, want=30", w.Header().Get("Retry-After"))
	}
	if w.Header().Get("X-RateLimit-Limit") != "1" {
		t.Errorf("X-RateLimit-Limit=%q", w.Header().Get("X-RateLimit-Limit"))
	}
	if !strings.Contains(w.Body.String(), "rate limit exceeded") {
		t.Errorf("body=%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "acc1") {
		t.Errorf("violated key should appear in error: body=%s", w.Body.String())
	}
}

func TestLimit_StoreError_500(t *testing.T) {
	pol := makePolicy(1, map[string]any{"default": map[string]any{"rpm": 60}})
	qp := &stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)

	store := &stubStore{reserveErr: errStub("redis down")}
	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), nil),
		Limit(LimitDeps{Store: store, Policies: pc}),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 500 {
		t.Fatalf("status=%d, want=500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "redis down") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestLimit_PolicyError_500(t *testing.T) {
	qp := &stubQPProvider{err: errStub("policy db down")}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)
	store := newStubStoreAllowAll()

	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), nil),
		Limit(LimitDeps{Store: store, Policies: pc}),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 500 {
		t.Fatalf("status=%d, want=500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ratelimit: build") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestLimit_OK_WritesXRateLimitHeaders(t *testing.T) {
	pol := makePolicy(1, map[string]any{"default": map[string]any{"rpm": 60}})
	qp := &stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)

	store := newStubStoreAllowAll()
	store.snapshot = ratelimit.BucketState{
		Limit:     60,
		Used:      5,
		Remaining: 55,
		ResetAt:   time.Now().Add(30 * time.Second),
	}

	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), nil),
		Limit(LimitDeps{Store: store, Policies: pc}),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-RateLimit-Limit") != "60" {
		t.Errorf("X-RateLimit-Limit=%q", w.Header().Get("X-RateLimit-Limit"))
	}
	if w.Header().Get("X-RateLimit-Remaining") != "55" {
		t.Errorf("X-RateLimit-Remaining=%q", w.Header().Get("X-RateLimit-Remaining"))
	}
}

func TestLimit_PostAdjust_PositiveDelta(t *testing.T) {
	pol := makePolicy(1, map[string]any{"default": map[string]any{"tpm": 100000}})
	qp := &stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)
	store := newStubStoreAllowAll()

	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), nil),
		Limit(LimitDeps{Store: store, Policies: pc}),
	)
	// 模拟下游 handler 实际产生了 usage（M7 写 rc.Usage）
	r.POST("/x", func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Usage = &domain.Usage{Total: 9999} // 大于 ReservedTPM 触发 + delta
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if store.adjustCalls.Load() != 1 {
		t.Fatalf("AdjustBatch calls=%d, want=1", store.adjustCalls.Load())
	}
	got := store.adjustCaptured[0]
	if len(got) != 1 || got[0].Delta <= 0 {
		t.Errorf("adjust=%+v, want positive delta", got)
	}
}

func TestLimit_PostAdjust_NegativeDelta(t *testing.T) {
	// reserved 估值 = body len / 4 + 4096；实际 Usage 很小 → 负 delta（退款）
	pol := makePolicy(1, map[string]any{"default": map[string]any{"tpm": 100000}})
	qp := &stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)
	store := newStubStoreAllowAll()

	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), nil),
		Limit(LimitDeps{Store: store, Policies: pc}),
	)
	r.POST("/x", func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Usage = &domain.Usage{Total: 5} // 远小于 ReservedTPM
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if store.adjustCalls.Load() != 1 {
		t.Fatalf("AdjustBatch calls=%d, want=1", store.adjustCalls.Load())
	}
	if store.adjustCaptured[0][0].Delta >= 0 {
		t.Errorf("delta=%d, want negative", store.adjustCaptured[0][0].Delta)
	}
}

func TestLimit_PostAdjust_NoUsage_Skip(t *testing.T) {
	pol := makePolicy(1, map[string]any{"default": map[string]any{"tpm": 100000}})
	qp := &stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)
	store := newStubStoreAllowAll()

	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), nil),
		Limit(LimitDeps{Store: store, Policies: pc}),
	)
	r.POST("/x", func(c *gin.Context) {
		// 不写 rc.Usage（M7 全失败场景）
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if store.adjustCalls.Load() != 0 {
		t.Errorf("AdjustBatch should be skipped when rc.Usage=nil, got %d", store.adjustCalls.Load())
	}
}

func TestLimit_PostAdjust_NoTPMKeys_Skip(t *testing.T) {
	// 只配 RPM，没 TPM → TPMBucketKeys 为空
	pol := makePolicy(1, map[string]any{"default": map[string]any{"rpm": 60}})
	qp := &stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)
	store := newStubStoreAllowAll()

	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), nil),
		Limit(LimitDeps{Store: store, Policies: pc}),
	)
	r.POST("/x", func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Usage = &domain.Usage{Total: 100}
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if store.adjustCalls.Load() != 0 {
		t.Errorf("no TPM keys → AdjustBatch skipped, got %d calls", store.adjustCalls.Load())
	}
}

func TestLimit_EnsureTPMEstimate_Idempotent(t *testing.T) {
	rc := newTestRC("gpt-4o")
	rc.RateLimit = nil

	cost1 := EnsureTPMEstimate(rc, []byte(`{"max_tokens":256}`))
	cost2 := EnsureTPMEstimate(rc, []byte(`{"max_tokens":999}`)) // 应当被忽略
	if cost1 == 0 {
		t.Fatal("first call returned 0")
	}
	if cost2 != cost1 {
		t.Errorf("second call cost=%d, want=%d (idempotent)", cost2, cost1)
	}
}

// =============================================================================
// errStub for inline error creation
// =============================================================================
type errStub string

func (e errStub) Error() string { return string(e) }
