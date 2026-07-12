package middleware

import (
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zereker/llm-gateway/internal/domain"
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

	if w.Code != 502 { // domain.ErrTransient -> DefaultHTTPStatus
		t.Errorf("status = %d, want 502", w.Code)
	}
	var body map[string]map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// new schema: code = stable machine code (upstream_error), class = behavioral category (transient)
	if body["error"]["class"] != "transient" {
		t.Errorf("class = %q, want transient", body["error"]["class"])
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

// TestRecover_PanicAfterWrite_DoesNotAppendErrorBody: if a panic happens
// *after* bytes have already been flushed to the client (e.g. a bug mid-stream
// on the M7 dispatch path, which writes directly to c.Writer), the recovery
// branch must NOT append its JSON error body onto the already-started
// response — that would corrupt an in-flight stream. Mirrors the Written()
// guard the rc.Error path already has.
func TestRecover_PanicAfterWrite_DoesNotAppendErrorBody(t *testing.T) {
	r := newGinTest(TraceContext(), Recover())
	r.GET("/stream-then-boom", func(c *gin.Context) {
		c.String(200, "partial stream chunk")
		// bytes are now on the wire; a later panic must not append to them
		panic("mid-stream explosion")
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/stream-then-boom", nil))

	if w.Code != 200 {
		t.Errorf("status = %d, want 200 (headers already sent before panic)", w.Code)
	}
	if got := w.Body.String(); got != "partial stream chunk" {
		t.Errorf("body = %q, want exactly the already-written chunk with no appended error JSON", got)
	}
	if strings.Contains(w.Body.String(), "internal server error") {
		t.Errorf("recovery appended an error body onto an already-written response: %s", w.Body.String())
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
