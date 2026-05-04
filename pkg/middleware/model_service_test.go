package middleware

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

type stubMSProvider struct {
	snap *domain.ModelServiceSnapshot
	err  error
}

func (s stubMSProvider) GetByModel(_ context.Context, _ string) (*domain.ModelServiceSnapshot, error) {
	return s.snap, s.err
}

func (s stubMSProvider) List(_ context.Context) ([]*domain.ModelServiceSnapshot, error) {
	if s.snap == nil {
		return nil, nil
	}
	return []*domain.ModelServiceSnapshot{s.snap}, nil
}

// stubSubscriptions M5 测试用：可控订阅判定结果。
type stubSubscriptions struct {
	has bool
	err error
}

func (s stubSubscriptions) Has(_ context.Context, _ string, _ int64) (bool, error) {
	return s.has, s.err
}

// stubPricing M5 测试用：可配 GetActive 返回值或 err。
type stubPricing struct {
	pv  *repo.PricingVersion
	err error
}

func (s stubPricing) GetActive(_ context.Context, _ string, _ int64, _ string, _ time.Time) (*repo.PricingVersion, error) {
	return s.pv, s.err
}
func (s stubPricing) ListHistory(_ context.Context, _ string, _ int64, _ string) ([]*repo.PricingVersion, error) {
	if s.pv == nil {
		return nil, nil
	}
	return []*repo.PricingVersion{s.pv}, nil
}

// installEnvelope 把一个最小 Envelope 塞进 RC，绕过 M3。
func installEnvelope(model string) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Envelope = &domain.RequestEnvelope{
			Parsed:         domain.CanonicalRequest{Model: model},
			SourceProtocol: domain.ProtoOpenAI,
			Modality:       domain.ModalityChat,
		}
		c.Next()
	}
}

func TestModelService_PopulatesRC(t *testing.T) {
	want := &domain.ModelServiceSnapshot{
		ID: 42, ServiceID: "svc", Model: "gpt-4o", UpdatedAt: time.Unix(1700000000, 0),
	}
	wantPV := &repo.PricingVersion{
		ID: 7, ModelServiceID: 42, RuleClass: "standard", EffectiveFrom: time.Unix(1700000000, 0),
	}
	r := newGinTest(
		TraceContext(), Recover(),
		installEnvelope("gpt-4o"),
		ModelService(ModelServiceDeps{
			Provider:      stubMSProvider{snap: want},
			Subscriptions: stubSubscriptions{has: true},
			Pricing:       stubPricing{pv: wantPV},
		}),
	)
	r.GET("/x", func(c *gin.Context) {
		rc := GetRequestContext(c)
		if rc.ModelService != want {
			t.Errorf("ModelService not set; got %+v", rc.ModelService)
		}
		if rc.Pricing.ModelServiceID != 42 {
			t.Errorf("Pricing.ModelServiceID = %d", rc.Pricing.ModelServiceID)
		}
		if rc.Pricing.PricingVersionID != 7 {
			t.Errorf("Pricing.PricingVersionID = %d, want 7", rc.Pricing.PricingVersionID)
		}
		if rc.Pricing.RuleClass != "standard" {
			t.Errorf("Pricing.RuleClass = %q, want standard", rc.Pricing.RuleClass)
		}
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 200 {
		t.Errorf("status = %d", w.Code)
	}
}

func TestModelService_ModelNotFound(t *testing.T) {
	r := newGinTest(
		TraceContext(), Recover(),
		installEnvelope("missing"),
		ModelService(ModelServiceDeps{
			Provider:      stubMSProvider{err: errors.New("nope")},
			Subscriptions: stubSubscriptions{},
			Pricing:       stubPricing{}, // 不会到 pricing
		}),
	)
	r.GET("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "model not found") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestModelService_NoPriceAborts503(t *testing.T) {
	want := &domain.ModelServiceSnapshot{ID: 42, ServiceID: "svc", Model: "gpt-4o"}
	r := newGinTest(
		TraceContext(), Recover(),
		installEnvelope("gpt-4o"),
		ModelService(ModelServiceDeps{
			Provider:      stubMSProvider{snap: want},
			Subscriptions: stubSubscriptions{has: true},
			Pricing:       stubPricing{err: errors.New("pricing: no active version for tenant=default model_service_id=42 class=standard at 2026-05-03T00:00:00Z")},
		}),
	)
	r.GET("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 503 {
		t.Errorf("status = %d, want 503 (no active price)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no active version") {
		t.Errorf("body should mention pricing: %s", w.Body.String())
	}
}

func TestModelService_RejectsMissingEnvelope(t *testing.T) {
	r := newGinTest(
		TraceContext(), Recover(),
		ModelService(ModelServiceDeps{
			Provider:      stubMSProvider{},
			Subscriptions: stubSubscriptions{},
			Pricing:       stubPricing{},
		}),
	)
	r.GET("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 500 {
		t.Errorf("status = %d, want 500 (envelope missing)", w.Code)
	}
}
