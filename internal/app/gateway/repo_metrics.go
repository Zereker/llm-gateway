package gateway

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zereker/llm-gateway/internal/metric"
	"github.com/zereker/llm-gateway/internal/repo"
)

// repoCacheMetrics bridges the repo.Metrics interface to a Prometheus counter.
//
// Metric docs/08 §3: llm_gateway_repo_cache_total{table, result}.
// result ∈ hit / miss / error.
type repoCacheMetrics struct {
	counter *prometheus.CounterVec
}

// newRepoCacheMetrics registers a Prometheus counter with the default registry.
//
// **Idempotent registration**: calling buildEngine a second time within the
// same process (e2e tests build an engine per case / future hot restarts)
// collides with the same-named collector already in the default registry.
// promauto.MustRegister would panic outright; here we use Register +
// AlreadyRegisteredError to reuse the existing collector, making buildEngine
// re-entrant. Any other registration error still fails fast via panic
// (surfacing a genuine metric-definition conflict).
func newRepoCacheMetrics() *repoCacheMetrics {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: metric.RepoCacheTotal,
		Help: "repo TTL cache lookups by table and result (hit / miss / error)",
	}, []string{"table", "result"})

	if err := prometheus.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if existing, ok := are.ExistingCollector.(*prometheus.CounterVec); ok {
				c = existing
			}
		} else {
			panic(err)
		}
	}
	return &repoCacheMetrics{counter: c}
}

func (m *repoCacheMetrics) Record(table, result string) {
	m.counter.WithLabelValues(table, result).Inc()
}

// Compile-time assertion: satisfies repo.Metrics.
var _ repo.Metrics = (*repoCacheMetrics)(nil)
