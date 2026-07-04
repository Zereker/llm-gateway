package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// **回归（review HIGH#8）**：M10 Tracing 挂在 Recover 外层后，abort 路径
// （401/429/…）也必须产生 llm_gateway_http_requests_total——旧版挂链尾时
// abort 一律跳过 M10，撞库 / 限流风暴在请求指标里隐身。
func TestTracing_RunsOnAbortPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 生产链序（chat.go）：TraceContext → Tracing → Recover → (abort 的 middleware)
	r.GET("/x",
		TraceContext(),
		Tracing(), // 无 outbox / tracer——只验证 metric 收尾执行
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
		t.Errorf("abort 路径没有产生 http_requests_total：before=%v after=%v", before, after)
	}
}

// panic 路径：Recover（内层）恢复并写 500，Tracing（外层）收尾仍执行且看到 500。
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
		t.Errorf("panic 路径没有产生 http_requests_total(500)：before=%v after=%v", before, after)
	}
}

// counterValue 从 prometheus default registry 按 label 子集匹配取 counter 当前值；
// 不存在返回 0。
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

