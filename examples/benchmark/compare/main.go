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
	"sort"
	"strings"
)

const gatewayPath = "gateway"

type report struct {
	Version     string          `json:"runner_version"`
	GeneratedAt string          `json:"generated_at"`
	Environment map[string]any  `json:"environment"`
	Results     []result        `json:"results"`
	Overhead    []overhead      `json:"gateway_minus_direct"`
	Resilience  map[string]bool `json:"resilience"`
}

type result struct {
	Path              string                `json:"path"`
	Mode              string                `json:"mode"`
	ErrorRate         float64               `json:"error_rate"`
	RPS               float64               `json:"requests_per_second"`
	LatencyP95MS      float64               `json:"latency_p95_ms"`
	TTFBP95MS         float64               `json:"ttfb_p95_ms"`
	PeakActiveStreams int64                 `json:"peak_active_streams"`
	GatewayResources  *gatewayResourceUsage `json:"gateway_resources,omitempty"`
}

type gatewayResourceUsage struct {
	CPUSeconds        float64 `json:"cpu_seconds"`
	PeakResidentBytes float64 `json:"peak_resident_bytes"`
	PeakHeapBytes     float64 `json:"peak_go_heap_bytes"`
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
	summaryPath := flag.String("summary", "", "optional Markdown summary output path")
	title := flag.String("title", "LLM Gateway Benchmark Result", "Markdown summary title")
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
	if *summaryPath != "" {
		if err := os.WriteFile(*summaryPath, []byte(renderSummary(*title, candidate, failures)), 0o644); err != nil {
			fatal(fmt.Errorf("write summary: %w", err))
		}
	}
	if len(failures) == 0 {
		fmt.Println("benchmark regression check passed")
		return
	}
	for _, failure := range failures {
		fmt.Fprintln(os.Stderr, "benchmark regression:", failure)
	}
	os.Exit(1)
}

func renderSummary(title string, r report, failures []string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n", title)
	if len(failures) == 0 {
		out.WriteString("✅ Regression check passed.\n\n")
	} else {
		out.WriteString("❌ Regression check failed.\n\n")
	}
	fmt.Fprintf(&out, "**Measured environment:** `%v/%v` · Go %v · %v CPUs · concurrency %v\n\n",
		r.Environment["os"], r.Environment["arch"], r.Environment["go"],
		r.Environment["cpus"], r.Environment["concurrency"])
	fmt.Fprintf(&out, "Commit `%v` · generated %v\n\n", r.Environment["commit"], r.GeneratedAt)

	out.WriteString("## Gateway overhead\n\n")
	out.WriteString("| Mode | Throughput | p95 latency | p95 TTFB |\n")
	out.WriteString("|---|---:|---:|---:|\n")
	for _, item := range r.Overhead {
		fmt.Fprintf(&out, "| %s | %+.3f%% | %+.3f ms | %+.3f ms |\n", item.Mode, item.ThroughputPercent, item.LatencyP95MS, item.TTFBP95MS)
	}

	out.WriteString("\n## Gateway resources\n\n")
	out.WriteString("| Mode | Requests/s | p95 latency | Active streams | CPU | Peak RSS | Peak Go heap |\n")
	out.WriteString("|---|---:|---:|---:|---:|---:|---:|\n")
	for _, item := range r.Results {
		if item.Path != gatewayPath || item.GatewayResources == nil {
			continue
		}
		fmt.Fprintf(&out, "| %s | %.3f | %.3f ms | %d | %.3f s | %.1f MiB | %.1f MiB |\n",
			item.Mode, item.RPS, item.LatencyP95MS, item.PeakActiveStreams,
			item.GatewayResources.CPUSeconds, mib(item.GatewayResources.PeakResidentBytes), mib(item.GatewayResources.PeakHeapBytes))
	}

	out.WriteString("\n## Resilience\n\n")
	scenarios := make([]string, 0, len(r.Resilience))
	for scenario := range r.Resilience {
		scenarios = append(scenarios, scenario)
	}
	sort.Strings(scenarios)
	for _, scenario := range scenarios {
		status := "✅"
		if !r.Resilience[scenario] {
			status = "❌"
		}
		fmt.Fprintf(&out, "- %s `%s`\n", status, scenario)
	}
	if len(failures) > 0 {
		out.WriteString("\n## Regressions\n\n")
		for _, failure := range failures {
			fmt.Fprintf(&out, "- %s\n", failure)
		}
	}
	return out.String()
}

func mib(bytes float64) float64 { return bytes / 1024 / 1024 }

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
	if baseline.Version != candidate.Version {
		failures = append(failures, fmt.Sprintf("runner version %q does not match baseline %q", candidate.Version, baseline.Version))
	}
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
	candidateByMode := make(map[string]overhead, len(candidate.Overhead))
	for _, current := range candidate.Overhead {
		candidateByMode[current.Mode] = current
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
	for mode := range baselineByMode {
		if _, ok := candidateByMode[mode]; !ok {
			failures = append(failures, "candidate missing mode: "+mode)
		}
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
