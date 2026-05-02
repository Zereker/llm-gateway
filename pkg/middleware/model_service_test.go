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
		ID: 42, ServiceID: "svc", Model: "gpt-4o", UpdateTime: time.Unix(1700000000, 0),
	}
	r := newGinTest(
		TraceContext(), Recover(),
		installEnvelope("gpt-4o"),
		ModelService(ModelServiceDeps{Provider: stubMSProvider{snap: want}}),
	)
	r.GET("/x", func(c *gin.Context) {
		rc := GetRequestContext(c)
		if rc.ModelService != want {
			t.Errorf("ModelService not set; got %+v", rc.ModelService)
		}
		if rc.Pricing.ModelServiceID != 42 {
			t.Errorf("Pricing.ModelServiceID = %d", rc.Pricing.ModelServiceID)
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
		ModelService(ModelServiceDeps{Provider: stubMSProvider{err: errors.New("nope")}}),
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

func TestModelService_RejectsMissingEnvelope(t *testing.T) {
	r := newGinTest(
		TraceContext(), Recover(),
		ModelService(ModelServiceDeps{Provider: stubMSProvider{}}),
	)
	r.GET("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 500 {
		t.Errorf("status = %d, want 500 (envelope missing)", w.Code)
	}
}
