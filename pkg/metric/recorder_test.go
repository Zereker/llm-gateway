package metric

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// reset 抹掉本测试用到的 metric，避免不同 test case 之间因 prometheus default
// registry 共享导致 label keys 冲突 panic。
//
// Note: prometheus 不支持"unregister + 同名异 label 重注册"——所以测试里每个
// metric name 只能用一次 label 组合。这里用唯一 name 规避冲突。
func TestInc_RegistersAndIncrementsCounter(t *testing.T) {
	const name = "test_metric_inc_counter_total"
	Inc(name, "result", "ok")
	Inc(name, "result", "ok")
	Inc(name, "result", "err")

	got := counterValue(t, name, map[string]string{"result": "ok"})
	if got != 2 {
		t.Errorf("ok counter=%v, want=2", got)
	}
	got = counterValue(t, name, map[string]string{"result": "err"})
	if got != 1 {
		t.Errorf("err counter=%v, want=1", got)
	}
}

func TestObserve_RegistersAndRecordsHistogram(t *testing.T) {
	const name = "test_metric_observe_histogram_seconds"
	Observe(name, 0.25, "path", "/x")
	Observe(name, 1.5, "path", "/x")

	// Histogram 验证 sample count 累积即可。
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sampleCount uint64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if h := m.GetHistogram(); h != nil {
				sampleCount = h.GetSampleCount()
			}
		}
	}
	if sampleCount != 2 {
		t.Errorf("sample count=%d, want=2", sampleCount)
	}
}

func TestGauge_RegistersAndSets(t *testing.T) {
	const name = "test_metric_gauge_value"
	Gauge(name, 42, "kind", "queue")
	Gauge(name, 100, "kind", "queue") // overwrite

	mfs, _ := prometheus.DefaultGatherer.Gather()
	var val float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if g := m.GetGauge(); g != nil {
				val = g.GetValue()
			}
		}
	}
	if val != 100 {
		t.Errorf("gauge=%v, want=100", val)
	}
}

func TestSplitLabels_OddCountDropsTail(t *testing.T) {
	keys, vals := splitLabels([]string{"a", "1", "b", "2", "c"}) // 奇数
	if len(keys) != 2 || len(vals) != 2 {
		t.Errorf("len(keys)=%d, len(vals)=%d, want 2/2", len(keys), len(vals))
	}
	if keys[0] != "a" || vals[0] != "1" || keys[1] != "b" || vals[1] != "2" {
		t.Errorf("unexpected pairs: %v / %v", keys, vals)
	}
}

func TestSplitLabels_Empty(t *testing.T) {
	keys, vals := splitLabels(nil)
	if len(keys) != 0 || len(vals) != 0 {
		t.Errorf("expected empty, got %v / %v", keys, vals)
	}
}

func TestSecondsBuckets_Monotonic(t *testing.T) {
	for i := 1; i < len(secondsBuckets); i++ {
		if secondsBuckets[i] <= secondsBuckets[i-1] {
			t.Errorf("buckets[%d]=%v not strictly increasing from buckets[%d]=%v",
				i, secondsBuckets[i], i-1, secondsBuckets[i-1])
		}
	}
}

// counterValue 从 default registry 取 counter 指定 labels 的当前值。
func counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if matchLabels(m.GetLabel(), labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func matchLabels(have []*dto.LabelPair, want map[string]string) bool {
	if len(have) != len(want) {
		return false
	}
	for _, lp := range have {
		if want[lp.GetName()] != lp.GetValue() {
			return false
		}
	}
	return true
}
