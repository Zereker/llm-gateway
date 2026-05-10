package middleware

import (
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/pkg/domain"
)

func TestRecover_CatchesPanicReturns500(t *testing.T) {
	r := newGinTest(TraceContext(), Recover())
	r.GET("/boom", func(c *gin.Context) {
		panic("simulated explosion")
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/boom", nil))

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "internal server error") {
		t.Errorf("body missing message: %s", w.Body.String())
	}
	var parsed map[string]map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tid := parsed["error"]["trace_id"]; len(tid) != 32 {
		t.Errorf("trace_id = %q, want W3C 32-hex", tid)
	} else if _, err := hex.DecodeString(tid); err != nil {
		t.Errorf("trace_id = %q, not valid hex", tid)
	}
	if rid := parsed["error"]["request_id"]; !strings.HasPrefix(rid, "req_") {
		t.Errorf("request_id = %q, want req_ prefix", rid)
	}
}

func TestRecover_PassThroughOnSuccess(t *testing.T) {
	r := newGinTest(TraceContext(), Recover())
	r.GET("/ok", func(c *gin.Context) {
		c.JSON(200, gin.H{"hello": "world"})
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/ok", nil))

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"hello":"world"`) {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestRecover_WritesRCErrorWhenSet(t *testing.T) {
	r := newGinTest(TraceContext(), Recover())
	r.GET("/err", func(c *gin.Context) {
		rc := GetRequestContext(c)
		rc.Error = &domain.AdapterError{
			Class:           domain.ErrTransient,
			Message:         "upstream 502",
			UpstreamMessage: "connection reset",
		}
		// no JSON write
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/err", nil))

	if w.Code != 502 { // domain.ErrTransient → DefaultHTTPStatus
		t.Errorf("status = %d, want 502", w.Code)
	}
	var body map[string]map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["error"]["code"] != "transient" {
		t.Errorf("code = %q, want transient", body["error"]["code"])
	}
	if body["error"]["message"] != "upstream 502" {
		t.Errorf("message = %q", body["error"]["message"])
	}
}

func TestRecover_HonorsExplicitHTTPStatus(t *testing.T) {
	r := newGinTest(TraceContext(), Recover())
	r.GET("/err", func(c *gin.Context) {
		GetRequestContext(c).Error = &domain.AdapterError{
			Class:      domain.ErrPermanent,
			HTTPStatus: 418, // explicit override
			Message:    "I'm a teapot",
		}
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/err", nil))

	if w.Code != 418 {
		t.Errorf("status = %d, want 418 (HTTPStatus override)", w.Code)
	}
}

func TestRecover_DoesNotOverwriteWrittenResponse(t *testing.T) {
	r := newGinTest(TraceContext(), Recover())
	r.GET("/already-written", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
		// then set Error — Recover should NOT rewrite (Writer.Written() == true)
		GetRequestContext(c).Error = &domain.AdapterError{
			Class:   domain.ErrUnknown,
			Message: "should not be written",
		}
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/already-written", nil))

	if w.Code != 200 {
		t.Errorf("status = %d, want 200 (response already written)", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("body should be the first written JSON: %s", w.Body.String())
	}
}

func TestRecover_PanicWithoutRC_StillWrites500(t *testing.T) {
	// Recover registered without TraceContext → no RC available
	r := newGinTest(Recover())
	r.GET("/boom", func(c *gin.Context) {
		panic("no RC")
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/boom", nil))

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "internal server error") {
		t.Errorf("body = %s", w.Body.String())
	}
}
