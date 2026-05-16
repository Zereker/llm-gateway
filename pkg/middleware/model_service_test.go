package middleware

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// attachM5Inputs：M3 之后的状态（Identity + Envelope shell）。
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

func newMSOpts(ms *domain.ModelService) []ModelServiceOption {
	return []ModelServiceOption{
		WithModelCatalog(stubCatalog{ms: ms}),
		WithSubscriptionChecker(stubSubs{has: true}),
	}
}

func TestModelService_HappyPath_FillsRC(t *testing.T) {
	ms := &domain.ModelService{ID: 7, ServiceID: "svc1", Model: "gpt-4o"}

	var rc *domain.RequestContext
	r := newGinTest(
		TraceContext(), Recover(),
		attachM5Inputs("gpt-4o", "acc1"),
		ModelService(newMSOpts(ms)...),
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
}

func TestModelService_500_EnvelopeMissing(t *testing.T) {
	r := newGinTest(TraceContext(), Recover(), ModelService(newMSOpts(nil)...))
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
	r := newGinTest(
		TraceContext(), Recover(),
		attachM5Inputs("ghost", "acc1"),
		ModelService(WithModelCatalog(stubCatalog{ms: nil}), WithSubscriptionChecker(stubSubs{})),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 404 {
		t.Fatalf("status=%d, want=404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "model_not_found") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestModelService_403_NotSubscribed(t *testing.T) {
	ms := &domain.ModelService{ID: 1, Model: "gpt-4o"}
	r := newGinTest(
		TraceContext(), Recover(),
		attachM5Inputs("gpt-4o", "acc1"),
		ModelService(WithModelCatalog(stubCatalog{ms: ms}), WithSubscriptionChecker(stubSubs{has: false})),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 403 {
		t.Fatalf("status=%d, want=403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "model_not_subscribed") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestModelService_503_CatalogError(t *testing.T) {
	// SQL/dep failure → fail-closed 503（docs/01 §7）
	r := newGinTest(
		TraceContext(), Recover(),
		attachM5Inputs("x", "acc1"),
		ModelService(WithModelCatalog(stubCatalog{err: errors.New("db down")}), WithSubscriptionChecker(stubSubs{})),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 503 {
		t.Fatalf("status=%d, want=503 (dep failure)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "dependency_unavailable") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestModelService_503_SubscriptionError(t *testing.T) {
	ms := &domain.ModelService{ID: 1, Model: "x"}
	r := newGinTest(
		TraceContext(), Recover(),
		attachM5Inputs("x", "acc1"),
		ModelService(WithModelCatalog(stubCatalog{ms: ms}), WithSubscriptionChecker(stubSubs{err: errors.New("db down")})),
	)
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", nil))
	if w.Code != 503 {
		t.Fatalf("status=%d, want=503", w.Code)
	}
}
