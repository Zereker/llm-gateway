package repo

// Metrics lets external code observe cache hit rate (not tied to Prometheus).
//
// **Implementation responsibility**: cmd creates a prometheus.CounterVec at
// assembly time (label = table / result), wraps it in this interface, and
// feeds it to each cached wrapper.
//
// **result values**:
//   - "hit"   -- GetOrLoad hit the cache (loader not called)
//   - "miss"  -- GetOrLoad missed but the loader succeeded
//   - "error" -- the loader returned err
//
// The default implementation is NoopMetrics (no reporting when cmd doesn't
// wire one up); the metrics contract is in the docs/08 §3 table.
type Metrics interface {
	Record(table, result string)
}

// NoopMetrics never reports — used by unit tests / when metrics aren't enabled.
type NoopMetrics struct{}

func (NoopMetrics) Record(table, result string) {}
