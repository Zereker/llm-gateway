package middleware

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// attachM5Inputs sets up the post-M3 state (Identity + Envelope shell).
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
	// SQL/dep failure -> fail-closed 503 (docs/01 §7)
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

// =============================================================================
// Fallback chain resolution (moved up to M5; M7 consumes rc.ModelChain directly)
// =============================================================================

// mapCatalog returns a different *ModelService per model name, for chain tests.
type mapCatalog struct {
	items map[string]*domain.ModelService
	err   error
}

func (m mapCatalog) GetByModel(_ context.Context, model string) (*domain.ModelService, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.items[model], nil
}

// mapSubs returns a subscription verdict keyed by modelServiceID.
type mapSubs struct {
	subscribed map[int64]bool
	err        error
}

func (m mapSubs) HasModel(_ context.Context, _ string, msID int64) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	return m.subscribed[msID], nil
}

// extractChainModels extracts the model name slice from rc.ModelChain, for easier assertions.
func extractChainModels(rc *domain.RequestContext) []string {
	out := make([]string, len(rc.ModelChain))
	for i, ms := range rc.ModelChain {
		out[i] = ms.Model
	}
	return out
}

func runModelChain(t *testing.T, hdr string, catalog ModelCatalog, subs SubscriptionChecker) *domain.RequestContext {
	t.Helper()
	var rc *domain.RequestContext
	r := newGinTest(
		TraceContext(), Recover(),
		attachM5Inputs("gpt-4o", "acc1"),
		ModelService(WithModelCatalog(catalog), WithSubscriptionChecker(subs)),
	)
	r.POST("/x", func(c *gin.Context) {
		rc = GetRequestContext(c)
		c.Status(200)
	})
	req := httptest.NewRequest("POST", "/x", nil)
	if hdr != "" {
		req.Header.Set(HeaderGatewayFallbackModels, hdr)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	return rc
}

func TestModelChain_NoHeader_OnlyPrimary(t *testing.T) {
	primary := &domain.ModelService{ID: 1, Model: "gpt-4o"}
	rc := runModelChain(t, "",
		mapCatalog{items: map[string]*domain.ModelService{"gpt-4o": primary}},
		mapSubs{subscribed: map[int64]bool{1: true}},
	)
	if got := extractChainModels(rc); !sliceEq(got, []string{"gpt-4o"}) {
		t.Errorf("chain=%v, want=[gpt-4o]", got)
	}
}

func TestModelChain_AllFallbacksSubscribed(t *testing.T) {
	primary := &domain.ModelService{ID: 1, Model: "gpt-4o"}
	fb1 := &domain.ModelService{ID: 2, Model: "gpt-4-turbo"}
	fb2 := &domain.ModelService{ID: 3, Model: "claude-3"}
	rc := runModelChain(t, "gpt-4-turbo,claude-3",
		mapCatalog{items: map[string]*domain.ModelService{
			"gpt-4o":      primary,
			"gpt-4-turbo": fb1,
			"claude-3":    fb2,
		}},
		mapSubs{subscribed: map[int64]bool{1: true, 2: true, 3: true}},
	)
	if got := extractChainModels(rc); !sliceEq(got, []string{"gpt-4o", "gpt-4-turbo", "claude-3"}) {
		t.Errorf("chain=%v, want=[gpt-4o gpt-4-turbo claude-3]", got)
	}
}

func TestModelChain_DropsUnknownFallback(t *testing.T) {
	primary := &domain.ModelService{ID: 1, Model: "gpt-4o"}
	fb2 := &domain.ModelService{ID: 3, Model: "claude-3"}
	rc := runModelChain(t, "gpt-5-imagine,claude-3",
		mapCatalog{items: map[string]*domain.ModelService{
			"gpt-4o":   primary,
			"claude-3": fb2,
		}},
		mapSubs{subscribed: map[int64]bool{1: true, 3: true}},
	)
	if got := extractChainModels(rc); !sliceEq(got, []string{"gpt-4o", "claude-3"}) {
		t.Errorf("chain=%v, want=[gpt-4o claude-3]", got)
	}
}

func TestModelChain_DropsUnsubscribedFallback(t *testing.T) {
	primary := &domain.ModelService{ID: 1, Model: "gpt-4o"}
	fb1 := &domain.ModelService{ID: 2, Model: "gpt-4-turbo"}
	fb2 := &domain.ModelService{ID: 3, Model: "claude-3"}
	rc := runModelChain(t, "gpt-4-turbo,claude-3",
		mapCatalog{items: map[string]*domain.ModelService{
			"gpt-4o":      primary,
			"gpt-4-turbo": fb1,
			"claude-3":    fb2,
		}},
		mapSubs{subscribed: map[int64]bool{1: true, 3: true}}, // fb1 not subscribed
	)
	if got := extractChainModels(rc); !sliceEq(got, []string{"gpt-4o", "claude-3"}) {
		t.Errorf("chain=%v, want=[gpt-4o claude-3]", got)
	}
}

func TestModelChain_DropsPrimaryInFallback(t *testing.T) {
	primary := &domain.ModelService{ID: 1, Model: "gpt-4o"}
	fb1 := &domain.ModelService{ID: 2, Model: "gpt-4-turbo"}
	rc := runModelChain(t, "gpt-4o,gpt-4-turbo", // primary appears in the fallback list
		mapCatalog{items: map[string]*domain.ModelService{
			"gpt-4o":      primary,
			"gpt-4-turbo": fb1,
		}},
		mapSubs{subscribed: map[int64]bool{1: true, 2: true}},
	)
	if got := extractChainModels(rc); !sliceEq(got, []string{"gpt-4o", "gpt-4-turbo"}) {
		t.Errorf("chain=%v, want=[gpt-4o gpt-4-turbo]", got)
	}
}

func TestModelChain_DedupAndOrder(t *testing.T) {
	primary := &domain.ModelService{ID: 1, Model: "gpt-4o"}
	fb1 := &domain.ModelService{ID: 2, Model: "a"}
	fb2 := &domain.ModelService{ID: 3, Model: "b"}
	rc := runModelChain(t, " a , b , a , ", // dedup while preserving order + trim + skip blanks
		mapCatalog{items: map[string]*domain.ModelService{
			"gpt-4o": primary, "a": fb1, "b": fb2,
		}},
		mapSubs{subscribed: map[int64]bool{1: true, 2: true, 3: true}},
	)
	if got := extractChainModels(rc); !sliceEq(got, []string{"gpt-4o", "a", "b"}) {
		t.Errorf("chain=%v, want=[gpt-4o a b]", got)
	}
}

func TestModelChain_RespectsMaxFallback(t *testing.T) {
	items := map[string]*domain.ModelService{
		"gpt-4o": {ID: 1, Model: "gpt-4o"},
	}
	subs := map[int64]bool{1: true}
	hdr := "a,b,c,d,e"
	for i, m := range []string{"a", "b", "c", "d", "e"} {
		items[m] = &domain.ModelService{ID: int64(2 + i), Model: m}
		subs[int64(2+i)] = true
	}
	rc := runModelChain(t, hdr,
		mapCatalog{items: items},
		mapSubs{subscribed: subs},
	)
	want := []string{"gpt-4o", "a", "b", "c"} // 1 primary + MaxFallbackModels=3
	if got := extractChainModels(rc); !sliceEq(got, want) {
		t.Errorf("chain=%v, want=%v", got, want)
	}
}

func TestModelChain_FallbackCatalogErrSilentDrop(t *testing.T) {
	// this time the catalog only errors for the fallback model; primary is fine
	primary := &domain.ModelService{ID: 1, Model: "gpt-4o"}
	cat := perModelCatalog{
		ok:  map[string]*domain.ModelService{"gpt-4o": primary},
		err: map[string]error{"flaky": errors.New("transient")},
	}
	rc := runModelChain(t, "flaky",
		cat,
		mapSubs{subscribed: map[int64]bool{1: true}},
	)
	if got := extractChainModels(rc); !sliceEq(got, []string{"gpt-4o"}) {
		t.Errorf("chain=%v, want=[gpt-4o] (flaky should be silently dropped)", got)
	}
}

// perModelCatalog lets the catalog return a different result (including an err) per model.
type perModelCatalog struct {
	ok  map[string]*domain.ModelService
	err map[string]error
}

func (p perModelCatalog) GetByModel(_ context.Context, model string) (*domain.ModelService, error) {
	if e, has := p.err[model]; has {
		return nil, e
	}
	return p.ok[model], nil
}
