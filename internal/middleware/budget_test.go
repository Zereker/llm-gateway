package middleware

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/internal/domain"
)

// attachIdentity is a small helper to inject an Identity (M4 only looks at SubAccountID).
func attachIdentity(sub string) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Identity = domain.UserIdentity{SubAccountID: sub}
		c.Next()
	}
}

func TestBudget_NilGate_PassThrough(t *testing.T) {
	r := newGinTest(TraceContext(), Recover(), attachIdentity("u1"), Budget(WithBudgetGate(nil)))
	r.GET("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 200 {
		t.Errorf("status=%d, want=200", w.Code)
	}
}

func TestBudget_Active_PassThrough(t *testing.T) {
	gate := &stubBudgetGate{status: domain.BudgetActive}
	r := newGinTest(TraceContext(), Recover(), attachIdentity("u1"), Budget(WithBudgetGate(gate)))
	r.GET("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gate.calls.Load() != 1 {
		t.Errorf("gate calls=%d, want=1", gate.calls.Load())
	}
}

func TestBudget_Inactive_402_Permanent(t *testing.T) {
	gate := &stubBudgetGate{status: domain.BudgetInactive}
	r := newGinTest(TraceContext(), Recover(), attachIdentity("u1"), Budget(WithBudgetGate(gate)))
	r.GET("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 402 {
		t.Fatalf("status=%d, want=402", w.Code)
	}
	if !strings.Contains(w.Body.String(), "budget inactive") {
		t.Errorf("body=%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"class":"permanent"`) {
		t.Errorf("expected class=permanent, body=%s", w.Body.String())
	}
}

func TestBudget_GateError_502_Unknown(t *testing.T) {
	gate := &stubBudgetGate{err: errors.New("billing down")}
	r := newGinTest(TraceContext(), Recover(), attachIdentity("u1"), Budget(WithBudgetGate(gate)))
	r.GET("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 502 {
		t.Fatalf("status=%d, want=502", w.Code)
	}
	// The internal gate error ("billing down") must NOT leak into the client
	// body — only a generic message (details go to logs).
	if strings.Contains(w.Body.String(), "billing down") {
		t.Errorf("internal error detail leaked to client body: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "budget check unavailable") {
		t.Errorf("expected generic message, body=%s", w.Body.String())
	}
}

func TestBudget_PassesSubAccountIDToGate(t *testing.T) {
	gate := &stubBudgetGate{status: domain.BudgetActive}
	r := newGinTest(TraceContext(), Recover(), attachIdentity("alice"), Budget(WithBudgetGate(gate)))
	r.GET("/x", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if gate.lastUser != "alice" {
		t.Errorf("gate received subAccountID=%q, want=alice", gate.lastUser)
	}
}

// keep the import: avoids an unused-context warning
var _ = context.Background
