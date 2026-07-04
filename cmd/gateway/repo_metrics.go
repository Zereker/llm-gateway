package main

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"

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

// newRepoCacheMetrics 注册 Prom counter 到默认 registry。
//
// **幂等注册**：同进程内二次 buildEngine（e2e 测试逐 case 建 engine / 未来热重启）
// 会撞到 default registry 里已存在的同名 collector。promauto.MustRegister 那会
// 直接 panic；这里用 Register + AlreadyRegisteredError 复用已有 collector，让
// buildEngine 可重入。其它注册错误仍 fail-fast panic（暴露真正的指标定义冲突）。
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

// 编译期断言：满足 repo.Metrics。
var _ repo.Metrics = (*repoCacheMetrics)(nil)
