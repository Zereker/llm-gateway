package metric

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Inc / Observe / Gauge are the lightweight facade used by business-side code:
//
//	metric.Inc(metric.AuthTotal, "result", "ok")
//	metric.Observe(metric.HTTPRequestDurationSeconds, secs, "path", "/v1/chat/completions")
//
// labels is a flat list of key,value pairs (an odd count, or an unpaired trailing
// value, is dropped).
//
// **Naming convention**: name uses the Prometheus-native underscore format directly
// (see names.go); this package does no string rewriting — the const literal is exactly
// the metric name shown on the Prometheus side.
//
// Implementation notes:
//   - The first call for a given metric name registers it to the Prometheus default
//     registry with whatever label keys were passed at that time; subsequent calls must
//     use the same label keys (otherwise Prometheus panics — this is a programming error).
//   - Histogram buckets are preset in seconds for the LLM latency range (see
//     secondsBuckets); if what you're measuring isn't latency, use promauto.NewHistogramVec
//     from the prometheus package directly with custom buckets.
//
// The /metrics endpoint is exposed by internal/router/helpers.go, reading the default registry
// directly via promhttp.Handler().

var (
	mu       sync.Mutex
	counters = make(map[string]*prometheus.CounterVec)
	histos   = make(map[string]*prometheus.HistogramVec)
	gauges   = make(map[string]*prometheus.GaugeVec)
)

// secondsBuckets: an exponential distribution from 1ms to 120s, covering the P50 ~ P99
// latency range of LLM requests. LLM streaming with long output can take 30s+, so the
// upper end extends to 120s to keep P99 accurate.
var secondsBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
	1, 2.5, 5, 10, 30, 60, 120,
}

// Inc increments the counter by 1.
func Inc(name string, labels ...string) {
	keys, vals := splitLabels(labels)
	getCounter(name, keys).WithLabelValues(vals...).Inc()
}

// Add accumulates an arbitrary value onto the counter (used for "numeric count" cases
// such as usage tokens).
func Add(name string, val float64, labels ...string) {
	if val <= 0 {
		return
	}
	keys, vals := splitLabels(labels)
	getCounter(name, keys).WithLabelValues(vals...).Add(val)
}

// Observe records one observed value (histogram, default second-scale buckets).
func Observe(name string, val float64, labels ...string) {
	keys, vals := splitLabels(labels)
	getHistogram(name, keys).WithLabelValues(vals...).Observe(val)
}

// Gauge sets a gauge value.
func Gauge(name string, val float64, labels ...string) {
	keys, vals := splitLabels(labels)
	getGauge(name, keys).WithLabelValues(vals...).Set(val)
}

// splitLabels splits [k1,v1,k2,v2,...] into keys / values; an odd trailing element is dropped.
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
