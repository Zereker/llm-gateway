package main

import (
	"os"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	baseline := report{Version: "2", Overhead: []overhead{{Mode: "stream", ThroughputPercent: -10, LatencyP95MS: 2, TTFBP95MS: 1}}}
	passing := report{
		Version:    "2",
		Results:    []result{{Path: "gateway", Mode: "stream"}},
		Overhead:   []overhead{{Mode: "stream", ThroughputPercent: -20, LatencyP95MS: 6, TTFBP95MS: 5}},
		Resilience: map[string]bool{"slow_client": true},
	}
	if failures := compare(baseline, passing, 5, .5, 20); len(failures) != 0 {
		t.Fatalf("passing report failed: %v", failures)
	}

	failing := passing
	failing.Results = []result{{Path: "gateway", Mode: "stream", ErrorRate: .01}}
	failing.Overhead = []overhead{{Mode: "stream", ThroughputPercent: -40, LatencyP95MS: 8, TTFBP95MS: 7}}
	failing.Resilience = map[string]bool{"slow_client": false}
	if failures := compare(baseline, failing, 5, .5, 20); len(failures) != 5 {
		t.Fatalf("failure count = %d, want 5: %v", len(failures), failures)
	}
}

func TestVersionedBaselineSummaryIsCurrent(t *testing.T) {
	baseline, err := load("../baselines/reference.json")
	if err != nil {
		t.Fatal(err)
	}
	want := renderSummary("Reference Performance Baseline", baseline, nil)
	got, err := os.ReadFile("../baselines/README.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatal("baselines/README.md is stale; regenerate it with `make -C examples/benchmark baseline`")
	}
}

func TestRenderSummary(t *testing.T) {
	r := report{
		Version:     "2",
		GeneratedAt: "2026-07-14T09:51:26Z",
		Environment: map[string]any{"commit": "abc123", "os": "linux", "arch": "amd64", "go": "go1.25", "cpus": 4, "concurrency": 20},
		Overhead:    []overhead{{Mode: "stream", ThroughputPercent: -2.004, LatencyP95MS: 4.274, TTFBP95MS: 2.703}},
		Results: []result{{Path: "gateway", Mode: "stream", RPS: 140.765, LatencyP95MS: 146.345, PeakActiveStreams: 20,
			GatewayResources: &gatewayResourceUsage{CPUSeconds: .32, PeakResidentBytes: 44 * 1024 * 1024, PeakHeapBytes: 13 * 1024 * 1024}}},
		Resilience: map[string]bool{"slow_client_completes": true},
	}
	summary := renderSummary("Reference Performance Baseline", r, nil)
	for _, want := range []string{"# Reference Performance Baseline", "✅ Regression check passed", "**Measured environment:** `linux/amd64` · Go go1.25 · 4 CPUs · concurrency 20", "Commit `abc123` · generated 2026-07-14T09:51:26Z", "| stream | -2.004% | +4.274 ms | +2.703 ms |", "44.0 MiB", "`slow_client_completes`"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}
