package middleware

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
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
	body := w.Body.String()
	if !strings.Contains(body, "internal server error") {
		t.Errorf("body missing message: %s", body)
	}
	if !strings.Contains(body, `"trace_id":"tr_`) {
		t.Errorf("body missing trace_id: %s", body)
	}
	if !strings.Contains(body, `"request_id":"req_`) {
		t.Errorf("body missing request_id: %s", body)
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
