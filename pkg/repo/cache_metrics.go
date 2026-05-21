package repo

// Metrics 让外部观测 cache 命中率（不绑死 Prometheus）。
//
// **实现责任**：cmd 在装配期创建一个 prometheus.CounterVec（label = table / result），
// wrap 成本接口，喂给每个 cached wrapper。
//
// **result 维度**：
//   - "hit"  ── GetOrLoad 命中缓存（未调 loader）
//   - "miss" ── GetOrLoad miss 但 loader 成功
//   - "error" ── loader 返 err
//
// 默认实现 NoopMetrics（cmd 没装时不报）；指标契约见 docs/08 §3 表格。
type Metrics interface {
	Record(table, result string)
}

// NoopMetrics 永不报告——给单测 / 不开启指标时用。
type NoopMetrics struct{}

func (NoopMetrics) Record(table, result string) {}
