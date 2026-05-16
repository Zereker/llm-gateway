package middleware

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestTimeout_NoDefault_NoHeader_NoOp(t *testing.T) {
	r := newGinTest(Timeout(0))
	var hasDeadline bool
	r.GET("/x", func(c *gin.Context) {
		_, ok := c.Request.Context().Deadline()
		hasDeadline = ok
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if hasDeadline {
		t.Errorf("ctx unexpectedly has deadline")
	}
}

func TestTimeout_DefaultApplied(t *testing.T) {
	r := newGinTest(Timeout(50 * time.Millisecond))
	var dur time.Duration
	r.GET("/x", func(c *gin.Context) {
		d, ok := c.Request.Context().Deadline()
		if ok {
			dur = time.Until(d)
		}
		c.Status(200)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if dur <= 0 || dur > 50*time.Millisecond {
		t.Errorf("deadline=%v, want <=50ms", dur)
	}
}

func TestTimeout_HeaderTighter_Overrides(t *testing.T) {
	r := newGinTest(Timeout(60 * time.Second))
	var dur time.Duration
	r.GET("/x", func(c *gin.Context) {
		d, _ := c.Request.Context().Deadline()
		dur = time.Until(d)
		c.Status(200)
	})

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(HeaderGatewayTimeout, "100ms")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if dur <= 0 || dur > 100*time.Millisecond {
		t.Errorf("deadline=%v, want <=100ms (header overrides looser default)", dur)
	}
}

func TestTimeout_HeaderLonger_Ignored(t *testing.T) {
	r := newGinTest(Timeout(100 * time.Millisecond))
	var dur time.Duration
	r.GET("/x", func(c *gin.Context) {
		d, _ := c.Request.Context().Deadline()
		dur = time.Until(d)
		c.Status(200)
	})

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(HeaderGatewayTimeout, "10s")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if dur > 100*time.Millisecond {
		t.Errorf("deadline=%v, want <=100ms (header longer than default must be ignored)", dur)
	}
}

func TestTimeout_HeaderMalformed_FallbackToDefault(t *testing.T) {
	r := newGinTest(Timeout(75 * time.Millisecond))
	var dur time.Duration
	r.GET("/x", func(c *gin.Context) {
		d, _ := c.Request.Context().Deadline()
		dur = time.Until(d)
		c.Status(200)
	})

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(HeaderGatewayTimeout, "not a duration")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if dur <= 0 || dur > 75*time.Millisecond {
		t.Errorf("deadline=%v, want <=75ms (malformed header falls back)", dur)
	}
}

func TestTimeout_NegativeHeaderIgnored(t *testing.T) {
	r := newGinTest(Timeout(50 * time.Millisecond))
	var dur time.Duration
	r.GET("/x", func(c *gin.Context) {
		d, _ := c.Request.Context().Deadline()
		dur = time.Until(d)
		c.Status(200)
	})

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(HeaderGatewayTimeout, "-5s")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if dur <= 0 || dur > 50*time.Millisecond {
		t.Errorf("deadline=%v, want <=50ms (negative header ignored)", dur)
	}
}

// 防止意外引入 gin import 没用
var _ = gin.New
