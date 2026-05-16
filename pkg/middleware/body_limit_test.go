package middleware

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestBodyLimit_ZeroOrNeg_PassThrough(t *testing.T) {
	for _, max := range []int64{0, -1} {
		r := newGinTest(BodyLimit(max))
		r.POST("/x", func(c *gin.Context) {
			body, _ := io.ReadAll(c.Request.Body)
			c.String(200, string(body))
		})

		w := httptest.NewRecorder()
		// 1MB body 必须能通过
		big := strings.Repeat("a", 1024*1024)
		r.ServeHTTP(w, httptest.NewRequest("POST", "/x", strings.NewReader(big)))
		if w.Code != 200 {
			t.Fatalf("max=%d: status=%d", max, w.Code)
		}
		if len(w.Body.String()) != len(big) {
			t.Errorf("max=%d: body trimmed: got %d want %d", max, len(w.Body.String()), len(big))
		}
	}
}

func TestBodyLimit_UnderLimit_OK(t *testing.T) {
	r := newGinTest(BodyLimit(100))
	r.POST("/x", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.String(500, err.Error())
			return
		}
		c.String(200, string(body))
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", strings.NewReader("hello")))
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if w.Body.String() != "hello" {
		t.Errorf("body=%q", w.Body.String())
	}
}

func TestBodyLimit_OverLimit_ReadFails(t *testing.T) {
	r := newGinTest(BodyLimit(5))
	var readErr error
	r.POST("/x", func(c *gin.Context) {
		_, readErr = io.ReadAll(c.Request.Body)
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/x", strings.NewReader("this is way too long")))
	if readErr == nil {
		t.Fatal("expected read error from MaxBytesReader")
	}
}
