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

func i64(v int64) *int64 { return &v }

func makePolicy(id int64, rule map[string]any) *repo.QuotaPolicy {
	js, _ := json.Marshal(rule)
	return &repo.QuotaPolicy{ID: id, Name: "p", RuleJSON: js, Enabled: true}
}

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
			RawBytes:       []byte(`{"model":"` + model + `"}`),
		}
		rc.ModelService = &domain.ModelService{ID: 1, Model: model}
		c.Next()
	}
}

func TestLimit_500_M3orM5Missing(t *testing.T) {
	r := newGinTest(TraceContext(), Recover(),
		Limit(WithLimitStore(newStubStoreAllowAll()), WithLimitPolicies(ratelimit.NewPolicyCache(&stubQPProvider{}, time.Minute))),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 500 {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestLimit_NoPolicy_PassThrough(t *testing.T) {
	store := newStubStoreAllowAll()
	pc := ratelimit.NewPolicyCache(&stubQPProvider{}, time.Minute)
	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", nil, nil),
		Limit(WithLimitStore(store), WithLimitPolicies(pc)),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if store.reserveCalls.Load() != 0 {
		t.Errorf("no policy → ReserveBatch should not be called, got %d", store.reserveCalls.Load())
	}
}

func TestLimit_RPMOnly_Reserves(t *testing.T) {
	pol := makePolicy(1, map[string]any{
		"default": map[string]any{"rpm": 60},
	})
	qp := &stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)
	store := newStubStoreAllowAll()

	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), nil),
		Limit(WithLimitStore(store), WithLimitPolicies(pc)),
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
	bs := store.reserveCaptured[0]
	if len(bs) != 1 || !strings.HasSuffix(bs[0].Key, ":rpm") {
		t.Errorf("expected single RPM bucket, got %+v", bs)
	}
}

func TestLimit_TPM_NotInReserve(t *testing.T) {
	// docs/04 §7：TPM 不预扣，不进 ReserveBatch
	pol := makePolicy(1, map[string]any{
		"default": map[string]any{"tpm": 100000},
	})
	qp := &stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)
	store := newStubStoreAllowAll()

	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), nil),
		Limit(WithLimitStore(store), WithLimitPolicies(pc)),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// 只有 TPM 没 RPM/RPS → ReserveBatch 不应被调（reserve buckets 为空）
	if store.reserveCalls.Load() != 0 {
		t.Errorf("TPM-only policy should NOT call ReserveBatch, got %d", store.reserveCalls.Load())
	}
}

func TestLimit_TwoLayer_Additive(t *testing.T) {
	accPol := makePolicy(1, map[string]any{"default": map[string]any{"rpm": 60}})
	akPol := makePolicy(2, map[string]any{
		"default":   map[string]any{"rpm": 30},
		"per_model": map[string]any{"gpt-4o": map[string]any{"rpm": 10}},
	})
	qp := &stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: accPol, 2: akPol}}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)
	store := newStubStoreAllowAll()

	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), i64(2)),
		Limit(WithLimitStore(store), WithLimitPolicies(pc)),
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

func TestLimit_Violated_429_WithRetryAfterAndDetails(t *testing.T) {
	pol := makePolicy(1, map[string]any{"default": map[string]any{"rpm": 1}})
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
		Limit(WithLimitStore(store), WithLimitPolicies(pc)),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 429 {
		t.Fatalf("status=%d, want=429", w.Code)
	}
	if w.Header().Get("Retry-After") != "30" {
		t.Errorf("Retry-After=%q", w.Header().Get("Retry-After"))
	}
	// 不应该写 X-RateLimit-* headers（docs/04 §9）
	if w.Header().Get("X-RateLimit-Limit") != "" {
		t.Errorf("X-RateLimit-Limit should NOT be set (docs/04 §9)")
	}
	if !strings.Contains(w.Body.String(), "rate_limit_exceeded") {
		t.Errorf("body=%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "bucket_key") {
		t.Errorf("violated bucket should appear in error details: body=%s", w.Body.String())
	}
}

func TestLimit_StoreError_503(t *testing.T) {
	pol := makePolicy(1, map[string]any{"default": map[string]any{"rpm": 60}})
	qp := &stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)

	store := &stubStore{reserveErr: errStub("redis down")}
	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), nil),
		Limit(WithLimitStore(store), WithLimitPolicies(pc)),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	// docs/04 §8：fail-closed → 503
	if w.Code != 503 {
		t.Fatalf("status=%d, want=503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "dependency_unavailable") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestLimit_PostCharge_TPM_UsesUsageTotal(t *testing.T) {
	pol := makePolicy(1, map[string]any{"default": map[string]any{"tpm": 100000}})
	qp := &stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)
	store := newStubStoreAllowAll()

	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), nil),
		Limit(WithLimitStore(store), WithLimitPolicies(pc)),
	)
	r.POST("/x", func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Usage = &domain.Usage{Total: 1234}
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if store.chargeCalls.Load() != 1 {
		t.Fatalf("ChargeBatch calls=%d, want=1", store.chargeCalls.Load())
	}
	bs := store.chargeCaptured[0]
	if len(bs) != 1 {
		t.Fatalf("charge buckets=%d, want=1", len(bs))
	}
	if bs[0].Cost != 1234 {
		t.Errorf("charge cost=%d, want=1234 (Usage.Total)", bs[0].Cost)
	}
}

func TestLimit_PostCharge_NoUsage_Skip(t *testing.T) {
	pol := makePolicy(1, map[string]any{"default": map[string]any{"tpm": 100000}})
	qp := &stubQPProvider{policies: map[int64]*repo.QuotaPolicy{1: pol}}
	pc := ratelimit.NewPolicyCache(qp, time.Minute)
	store := newStubStoreAllowAll()

	r := newGinTest(TraceContext(), Recover(),
		attachM6Inputs("gpt-4o", i64(1), nil),
		Limit(WithLimitStore(store), WithLimitPolicies(pc)),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) }) // no Usage

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if store.chargeCalls.Load() != 0 {
		t.Errorf("rc.Usage=nil → ChargeBatch skipped, got %d", store.chargeCalls.Load())
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }
