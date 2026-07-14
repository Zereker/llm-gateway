// Command compare checks a benchmark report against a versioned reference.
// It uses gateway-minus-direct deltas so configured upstream latency cancels
// out, and gives shared CI runners explicit absolute and relative tolerance.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
)

type report struct {
	Results    []result        `json:"results"`
	Overhead   []overhead      `json:"gateway_minus_direct"`
	Resilience map[string]bool `json:"resilience"`
}

type result struct {
	Path      string  `json:"path"`
	Mode      string  `json:"mode"`
	ErrorRate float64 `json:"error_rate"`
}

type overhead struct {
	Mode              string  `json:"mode"`
	ThroughputPercent float64 `json:"throughput_percent"`
	LatencyP95MS      float64 `json:"latency_p95_ms"`
	TTFBP95MS         float64 `json:"ttfb_p95_ms"`
}

func main() {
	baselinePath := flag.String("baseline", "/baselines/reference.json", "reference report")
	candidatePath := flag.String("candidate", "/results/latest.json", "candidate report")
	absTolerance := flag.Float64("absolute-tolerance-ms", 20, "minimum allowed p95 delta increase")
	relTolerance := flag.Float64("relative-tolerance", 1, "allowed p95 delta increase relative to baseline")
	throughputTolerance := flag.Float64("throughput-tolerance-points", 40, "allowed throughput percentage-point decrease")
	flag.Parse()

	baseline, err := load(*baselinePath)
	if err != nil {
		fatal(err)
	}
	candidate, err := load(*candidatePath)
	if err != nil {
		fatal(err)
	}

	failures := compare(baseline, candidate, *absTolerance, *relTolerance, *throughputTolerance)
	if len(failures) == 0 {
		fmt.Println("benchmark regression check passed")
		return
	}
	for _, failure := range failures {
		fmt.Fprintln(os.Stderr, "benchmark regression:", failure)
	}
	os.Exit(1)
}

func load(path string) (report, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return report{}, fmt.Errorf("read %s: %w", path, err)
	}
	var r report
	if err := json.Unmarshal(body, &r); err != nil {
		return report{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return r, nil
}

func compare(baseline, candidate report, absTolerance, relTolerance, throughputTolerance float64) []string {
	var failures []string
	for _, result := range candidate.Results {
		if result.ErrorRate > 0 {
			failures = append(failures, fmt.Sprintf("%s/%s error_rate %.4f > 0", result.Path, result.Mode, result.ErrorRate))
		}
	}
	for scenario, ok := range candidate.Resilience {
		if !ok {
			failures = append(failures, "resilience scenario failed: "+scenario)
		}
	}

	baselineByMode := make(map[string]overhead, len(baseline.Overhead))
	for _, item := range baseline.Overhead {
		baselineByMode[item.Mode] = item
	}
	for _, current := range candidate.Overhead {
		reference, ok := baselineByMode[current.Mode]
		if !ok {
			failures = append(failures, "baseline missing mode: "+current.Mode)
			continue
		}
		if current.ThroughputPercent < reference.ThroughputPercent-throughputTolerance {
			failures = append(failures, fmt.Sprintf("%s throughput overhead %.2f%% < allowed %.2f%%", current.Mode, current.ThroughputPercent, reference.ThroughputPercent-throughputTolerance))
		}
		failures = appendMetricFailure(failures, current.Mode+" latency_p95_ms", reference.LatencyP95MS, current.LatencyP95MS, absTolerance, relTolerance)
		failures = appendMetricFailure(failures, current.Mode+" ttfb_p95_ms", reference.TTFBP95MS, current.TTFBP95MS, absTolerance, relTolerance)
	}
	return failures
}

func appendMetricFailure(failures []string, name string, baseline, candidate, absTolerance, relTolerance float64) []string {
	limit := baseline + math.Max(absTolerance, math.Abs(baseline)*relTolerance)
	if candidate > limit {
		return append(failures, fmt.Sprintf("%s %.3f > allowed %.3f (baseline %.3f)", name, candidate, limit, baseline))
	}
	return failures
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "benchmark compare:", err)
	os.Exit(2)
}
