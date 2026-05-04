package metric

import (
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Inc / Observe / Gauge 是业务侧使用的轻量门面：
//
//	metric.Inc(metric.AuthTotal, "result", "ok")
//	metric.Observe(metric.HTTPRequestDurationMs, ms, "path", "/v1/chat/completions")
//
// labels 是 flat key,value pairs（奇数个或不成对会被丢掉尾值）。
//
// 实现说明：
//   - 同一 metric 名首次调用时按当时的 label keys 注册到 Prometheus default registry；
//     后续调用必须使用相同的 label keys（否则 Prometheus 会 panic——属于编程错）。
//   - 名字里的 `.` 自动替换成 `_`（Prometheus 命名规范）。
//   - histogram 桶按毫秒级响应时间预设；如果你测的不是延迟，自定义桶请直接用
//     prometheus 包的 promauto.NewHistogramVec。
//
// /metrics 端点由 pkg/router/helpers.go 暴露，走 promhttp.Handler() 直读 default registry。

var (
	mu       sync.Mutex
	counters = make(map[string]*prometheus.CounterVec)
	histos   = make(map[string]*prometheus.HistogramVec)
	gauges   = make(map[string]*prometheus.GaugeVec)
)

// msBuckets：1ms ~ 10s 的指数分布，覆盖 LLM 请求 P50 ~ P99 的延迟范围。
var msBuckets = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000}

// Inc 增加 1 次计数。
func Inc(name string, labels ...string) {
	keys, vals := splitLabels(labels)
	getCounter(name, keys).WithLabelValues(vals...).Inc()
}

// Observe 记录一次观测值（histogram，默认毫秒桶）。
func Observe(name string, val float64, labels ...string) {
	keys, vals := splitLabels(labels)
	getHistogram(name, keys).WithLabelValues(vals...).Observe(val)
}

// Gauge 设置一个 gauge 值。
func Gauge(name string, val float64, labels ...string) {
	keys, vals := splitLabels(labels)
	getGauge(name, keys).WithLabelValues(vals...).Set(val)
}

// promName 把 ai_gateway.foo.bar → ai_gateway_foo_bar；prom 不允许 `.`。
func promName(s string) string { return strings.ReplaceAll(s, ".", "_") }

// splitLabels 把 [k1,v1,k2,v2,...] 切成 keys / values；奇数个尾部丢弃。
func splitLabels(pairs []string) ([]string, []string) {
	n := len(pairs) / 2
	keys := make([]string, n)
	vals := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = pairs[2*i]
		vals[i] = pairs[2*i+1]
	}
	return keys, vals
}

func getCounter(name string, keys []string) *prometheus.CounterVec {
	pn := promName(name)
	mu.Lock()
	defer mu.Unlock()
	if cv, ok := counters[pn]; ok {
		return cv
	}
	cv := promauto.NewCounterVec(prometheus.CounterOpts{Name: pn}, keys)
	counters[pn] = cv
	return cv
}

func getHistogram(name string, keys []string) *prometheus.HistogramVec {
	pn := promName(name)
	mu.Lock()
	defer mu.Unlock()
	if hv, ok := histos[pn]; ok {
		return hv
	}
	hv := promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    pn,
		Buckets: msBuckets,
	}, keys)
	histos[pn] = hv
	return hv
}

func getGauge(name string, keys []string) *prometheus.GaugeVec {
	pn := promName(name)
	mu.Lock()
	defer mu.Unlock()
	if gv, ok := gauges[pn]; ok {
		return gv
	}
	gv := promauto.NewGaugeVec(prometheus.GaugeOpts{Name: pn}, keys)
	gauges[pn] = gv
	return gv
}
