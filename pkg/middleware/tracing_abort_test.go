package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// **Regression (review HIGH#8)**: once M10 Tracing is hung on Recover's outer layer,
// abort paths (401/429/...) must also produce llm_gateway_http_requests_total -- in
// the old version, hanging it at the end of the chain meant aborts always skipped
// M10, so credential-stuffing / rate-limit storms went invisible in request metrics.
func TestTracing_RunsOnAbortPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// production chain order (chat.go): TraceContext -> Tracing -> Recover -> (the aborting middleware)
	r.GET("/x",
		TraceContext(),
		Tracing(), // no outbox / tracer -- only verifies the metric tail-call runs
		Recover(),
		func(c *gin.Context) {
			abortWithCode(c, 429, domain.ErrRateLimit, domain.ErrCodeRateLimitExceeded, "quota exceeded")
		},
		func(c *gin.Context) { c.Status(200) },
	)

	before := counterValue(t, "llm_gateway_http_requests_total", map[string]string{"status": "429"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))

	if w.Code != 429 {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	after := counterValue(t, "llm_gateway_http_requests_total", map[string]string{"status": "429"})
	if after <= before {
		t.Errorf("the abort path did not produce http_requests_total: before=%v after=%v", before, after)
	}
}

// panic path: Recover (inner) recovers and writes 500; Tracing's (outer) tail-call
// still runs and observes the 500.
func TestTracing_RunsAfterRecoveredPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x",
		TraceContext(),
		Tracing(),
		Recover(),
		func(c *gin.Context) { panic("boom") },
	)

	before := counterValue(t, "llm_gateway_http_requests_total", map[string]string{"status": "500"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))

	if w.Code != 500 {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	after := counterValue(t, "llm_gateway_http_requests_total", map[string]string{"status": "500"})
	if after <= before {
		t.Errorf("the panic path did not produce http_requests_total(500): before=%v after=%v", before, after)
	}
}

// counterValue reads a counter's current value from the prometheus default registry,
// matching by a subset of labels; returns 0 if not found.
func counterValue(t *testing.T, name string, want map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sum float64
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			match := true
			for _, lp := range m.GetLabel() {
				if v, ok := want[lp.GetName()]; ok && v != lp.GetValue() {
					match = false
					break
				}
			}
			if match {
				sum += m.GetCounter().GetValue()
			}
		}
	}
	return sum
}
