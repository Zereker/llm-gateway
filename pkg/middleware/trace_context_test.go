package middleware

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newGinTest(handlers ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	for _, h := range handlers {
		r.Use(h)
	}
	return r
}

func TestTraceContext_GeneratesIDs(t *testing.T) {
	r := newGinTest(TraceContext())
	var gotTrace, gotRequest string
	r.GET("/x", func(c *gin.Context) {
		rc := GetRequestContext(c)
		gotTrace = rc.TraceID
		gotRequest = rc.RequestID
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))

	if !strings.HasPrefix(gotTrace, "tr_") {
		t.Errorf("trace = %q, want tr_ prefix", gotTrace)
	}
	if !strings.HasPrefix(gotRequest, "req_") {
		t.Errorf("request = %q, want req_ prefix", gotRequest)
	}
}

func TestTraceContext_HonorsXTraceIdHeader(t *testing.T) {
	r := newGinTest(TraceContext())
	var got string
	r.GET("/x", func(c *gin.Context) {
		got = GetRequestContext(c).TraceID
		c.Status(200)
	})

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Trace-Id", "tr_externalABC")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got != "tr_externalABC" {
		t.Errorf("got %q, want tr_externalABC", got)
	}
}

func TestTraceContext_FillsAllRCFields(t *testing.T) {
	r := newGinTest(TraceContext())
	r.GET("/x", func(c *gin.Context) {
		rc := GetRequestContext(c)
		switch {
		case rc.StartTime.IsZero():
			t.Error("StartTime zero")
		case rc.Ctx == nil:
			t.Error("Ctx nil")
		case rc.GinCtx == nil:
			t.Error("GinCtx nil")
		case rc.Logger == nil:
			t.Error("Logger nil")
		case rc.Extras == nil:
			t.Error("Extras nil")
		}
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
}
