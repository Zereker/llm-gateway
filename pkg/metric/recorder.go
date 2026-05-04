package metric

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Inc / Observe / Gauge 是业务侧使用的轻量门面：
//
//	metric.Inc(metric.AuthTotal, "result", "ok")
//	metric.Observe(metric.HTTPRequestDurationSeconds, secs, "path", "/v1/chat/completions")
//
// labels 是 flat key,value pairs（奇数个或不成对会被丢掉尾值）。
//
// **命名约定**：name 直接用 Prometheus-native 下划线格式（详见 names.go）；本包不做
// 任何 string rewrite——const 字面值就是 Prometheus 端展示的 metric name。
//
// 实现说明：
//   - 同一 metric 名首次调用时按当时的 label keys 注册到 Prometheus default registry；
//     后续调用必须使用相同的 label keys（否则 Prometheus 会 panic——属于编程错）。
//   - histogram 桶按 LLM 延迟范围预设秒级（见 secondsBuckets）；如果你测的不是延迟，
//     自定义桶请直接用 prometheus 包的 promauto.NewHistogramVec。
//
// /metrics 端点由 pkg/router/helpers.go 暴露，走 promhttp.Handler() 直读 default registry。

var (
	mu       sync.Mutex
	counters = make(map[string]*prometheus.CounterVec)
	histos   = make(map[string]*prometheus.HistogramVec)
	gauges   = make(map[string]*prometheus.GaugeVec)
)

// secondsBuckets：1ms ~ 120s 的指数分布，覆盖 LLM 请求 P50 ~ P99 延迟范围。
// LLM 流式长输出可达 30s+，所以右侧拖到 120s 防止 P99 算不准。
var secondsBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
	1, 2.5, 5, 10, 30, 60, 120,
}

// Inc 增加 1 次计数。
func Inc(name string, labels ...string) {
	keys, vals := splitLabels(labels)
	getCounter(name, keys).WithLabelValues(vals...).Inc()
}

// Observe 记录一次观测值（histogram，默认秒级桶）。
func Observe(name string, val float64, labels ...string) {
	keys, vals := splitLabels(labels)
	getHistogram(name, keys).WithLabelValues(vals...).Observe(val)
}

// Gauge 设置一个 gauge 值。
func Gauge(name string, val float64, labels ...string) {
	keys, vals := splitLabels(labels)
	getGauge(name, keys).WithLabelValues(vals...).Set(val)
}

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
	mu.Lock()
	defer mu.Unlock()
	if cv, ok := counters[name]; ok {
		return cv
	}
	cv := promauto.NewCounterVec(prometheus.CounterOpts{Name: name}, keys)
	counters[name] = cv
	return cv
}

func getHistogram(name string, keys []string) *prometheus.HistogramVec {
	mu.Lock()
	defer mu.Unlock()
	if hv, ok := histos[name]; ok {
		return hv
	}
	hv := promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    name,
		Buckets: secondsBuckets,
	}, keys)
	histos[name] = hv
	return hv
}

func getGauge(name string, keys []string) *prometheus.GaugeVec {
	mu.Lock()
	defer mu.Unlock()
	if gv, ok := gauges[name]; ok {
		return gv
	}
	gv := promauto.NewGaugeVec(prometheus.GaugeOpts{Name: name}, keys)
	gauges[name] = gv
	return gv
}
