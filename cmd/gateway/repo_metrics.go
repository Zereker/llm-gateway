package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/repo"
)

// repoCacheMetrics 把 repo.Metrics 接口桥接到 Prometheus counter。
//
// 指标 docs/08 §3：llm_gateway_repo_cache_total{table, result}。
// result ∈ hit / miss / error。
type repoCacheMetrics struct {
	counter *prometheus.CounterVec
}

// newRepoCacheMetrics 注册 Prom counter；进程级单例（promauto 用默认 registry）。
func newRepoCacheMetrics() *repoCacheMetrics {
	return &repoCacheMetrics{
		counter: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: metric.RepoCacheTotal,
			Help: "repo TTL cache lookups by table and result (hit / miss / error)",
		}, []string{"table", "result"}),
	}
}

func (m *repoCacheMetrics) Record(table, result string) {
	m.counter.WithLabelValues(table, result).Inc()
}

// 编译期断言：满足 repo.Metrics。
var _ repo.Metrics = (*repoCacheMetrics)(nil)
