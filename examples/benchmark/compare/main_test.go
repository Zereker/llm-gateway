package main

import "testing"

func TestCompare(t *testing.T) {
	baseline := report{Overhead: []overhead{{Mode: "stream", ThroughputPercent: -10, LatencyP95MS: 2, TTFBP95MS: 1}}}
	passing := report{
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
