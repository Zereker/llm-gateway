package middleware

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/repo"
)

// 注入 Identity + Envelope shell + RawBytes 的最简 chain（M5 上游契约）。
func attachM5Inputs(model, account string) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Identity = domain.UserIdentity{AccountID: account, SubAccountID: "sub"}
		rc.Envelope = &domain.RequestEnvelope{
			SourceProtocol: domain.ProtoOpenAI,
			Modality:       domain.ModalityChat,
			Model:          model,
			RawBytes:       []byte(`{"model":"` + model + `"}`),
		}
		c.Next()
	}
}

func defaultMSDeps(ms *repo.ModelService, pv *repo.PricingVersion) ModelServiceDeps {
	return ModelServiceDeps{
		Provider:      stubMSReader{ms: ms},
		Subscriptions: stubSubs{has: true},
		Pricing:       stubPricing{pv: pv},
	}
}

func TestModelService_HappyPath_FillsRCFields(t *testing.T) {
	ms := &repo.ModelService{ID: 7, ServiceID: "svc1", Model: "gpt-4o"}
	pv := &repo.PricingVersion{ID: 42, EffectiveFrom: time.Now()}

	var rc *domain.RequestContext
	r := newGinTest(
		TraceContext(), Recover(),
		attachM5Inputs("gpt-4o", "acc1"),
		ModelService(defaultMSDeps(ms, pv)),
	)
	r.POST("/x", func(c *gin.Context) {
		rc = GetRequestContext(c)
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if rc.ModelService == nil || rc.ModelService.ID != 7 {
		t.Errorf("rc.ModelService=%+v", rc.ModelService)
	}
	if rc.Pricing.ModelServiceID != 7 || rc.Pricing.PricingVersionID != 42 {
		t.Errorf("rc.Pricing=%+v", rc.Pricing)
	}
	if rc.Pricing.RuleClass != "standard" {
		t.Errorf("RuleClass=%q, want=standard", rc.Pricing.RuleClass)
	}
}

func TestModelService_500_EnvelopeMissing(t *testing.T) {
	r := newGinTest(TraceContext(), Recover(), ModelService(defaultMSDeps(nil, nil)))
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 500 {
		t.Fatalf("status=%d, want=500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "M3 Envelope did not run") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestModelService_404_ModelNotFound(t *testing.T) {
	deps := ModelServiceDeps{
		Provider:      stubMSReader{err: errors.New("not found")},
		Subscriptions: stubSubs{},
		Pricing:       stubPricing{},
	}
	r := newGinTest(
		TraceContext(), Recover(),
		attachM5Inputs("ghost", "acc1"),
		ModelService(deps),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 404 {
		t.Fatalf("status=%d, want=404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "model not found") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestModelService_403_NotSubscribed(t *testing.T) {
	ms := &repo.ModelService{ID: 1, Model: "gpt-4o"}
	deps := ModelServiceDeps{
		Provider:      stubMSReader{ms: ms},
		Subscriptions: stubSubs{has: false},
		Pricing:       stubPricing{},
	}
	r := newGinTest(
		TraceContext(), Recover(),
		attachM5Inputs("gpt-4o", "acc1"),
		ModelService(deps),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 403 {
		t.Fatalf("status=%d, want=403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not subscribed") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestModelService_500_SubscriptionLookupError(t *testing.T) {
	ms := &repo.ModelService{ID: 1, Model: "x"}
	deps := ModelServiceDeps{
		Provider:      stubMSReader{ms: ms},
		Subscriptions: stubSubs{err: errors.New("db down")},
		Pricing:       stubPricing{},
	}
	r := newGinTest(
		TraceContext(), Recover(),
		attachM5Inputs("x", "acc1"),
		ModelService(deps),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 500 {
		t.Fatalf("status=%d, want=500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "subscription lookup") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestModelService_503_PricingNoActive(t *testing.T) {
	ms := &repo.ModelService{ID: 1, Model: "x"}
	deps := ModelServiceDeps{
		Provider:      stubMSReader{ms: ms},
		Subscriptions: stubSubs{has: true},
		Pricing:       stubPricing{err: errors.New("no active version for x")},
	}
	r := newGinTest(
		TraceContext(), Recover(),
		attachM5Inputs("x", "acc1"),
		ModelService(deps),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 503 {
		t.Fatalf("status=%d, want=503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no active version") {
		t.Errorf("body=%s", w.Body.String())
	}
}
