package middleware

import (
	"encoding/hex"
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
		gotTrace = TraceIDFromCtx(rc.Ctx)
		gotRequest = rc.RequestID
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))

	if _, err := hex.DecodeString(gotTrace); err != nil || len(gotTrace) != 32 {
		t.Errorf("trace = %q, want W3C 32-hex trace_id", gotTrace)
	}
	if !strings.HasPrefix(gotRequest, "req_") {
		t.Errorf("request = %q, want req_ prefix", gotRequest)
	}
}

func TestTraceContext_HonorsXTraceIdHeader(t *testing.T) {
	r := newGinTest(TraceContext())
	var got string
	r.GET("/x", func(c *gin.Context) {
		got = TraceIDFromCtx(GetRequestContext(c).Ctx)
		c.Status(200)
	})

	const externalTraceID = "0102030405060708090a0b0c0d0e0f10"
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Trace-Id", externalTraceID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got != externalTraceID {
		t.Errorf("got %q, want %q", got, externalTraceID)
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
		case rc.Extras == nil:
			t.Error("Extras nil")
		}
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
}
