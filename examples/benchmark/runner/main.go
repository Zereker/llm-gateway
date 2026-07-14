// Command runner compares deterministic direct-upstream and gateway traffic.
// It uses only the Go standard library, so its behavior is pinned to this source
// and the repository's Go toolchain rather than an unversioned host utility.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type sample struct {
	Duration time.Duration
	TTFB     time.Duration
	Bytes    int64
	OK       bool
}

type result struct {
	Path              string                `json:"path"`
	Mode              string                `json:"mode"`
	Requests          int                   `json:"requests"`
	Errors            int                   `json:"errors"`
	ErrorRate         float64               `json:"error_rate"`
	RPS               float64               `json:"requests_per_second"`
	P50MS             float64               `json:"latency_p50_ms"`
	P95MS             float64               `json:"latency_p95_ms"`
	P99MS             float64               `json:"latency_p99_ms"`
	TTFBP50MS         float64               `json:"ttfb_p50_ms"`
	TTFBP95MS         float64               `json:"ttfb_p95_ms"`
	TTFBP99MS         float64               `json:"ttfb_p99_ms"`
	MeanBytes         float64               `json:"mean_bytes"`
	PeakActive        int64                 `json:"peak_active_requests"`
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
	LatencyP50MS      float64 `json:"latency_p50_ms"`
	LatencyP95MS      float64 `json:"latency_p95_ms"`
	LatencyP99MS      float64 `json:"latency_p99_ms"`
	TTFBP50MS         float64 `json:"ttfb_p50_ms"`
	TTFBP95MS         float64 `json:"ttfb_p95_ms"`
	TTFBP99MS         float64 `json:"ttfb_p99_ms"`
}

type report struct {
	Version     string          `json:"runner_version"`
	GeneratedAt string          `json:"generated_at"`
	Environment map[string]any  `json:"environment"`
	Results     []result        `json:"results"`
	Overhead    []overhead      `json:"gateway_minus_direct"`
	Resilience  map[string]bool `json:"resilience"`
}

func main() {
	direct := flag.String("direct", "http://benchmark-upstream:9092/v1/chat/completions", "direct upstream URL")
	gateway := flag.String("gateway", "http://gateway:8080/v1/chat/completions", "gateway URL")
	apiKey := flag.String("api-key", "sk-demo-llm-gateway", "gateway API key")
	requests := flag.Int("requests", 200, "requests per path and mode")
	concurrency := flag.Int("concurrency", 20, "workers per run")
	metricsURL := flag.String("gateway-metrics", "http://gateway:8080/metrics", "gateway Prometheus metrics URL")
	output := flag.String("output", "", "optional path for the JSON report")
	flag.Parse()

	client := &http.Client{Timeout: 30 * time.Second}
	r := report{
		Version:     "2",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Environment: map[string]any{"commit": os.Getenv("BENCHMARK_COMMIT_SHA"), "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "cpus": runtime.NumCPU(), "requests_per_case": *requests, "concurrency": *concurrency, "upstream_nonstream_delay_ms": 20, "upstream_first_token_delay_ms": 50, "upstream_chunk_delay_ms": 10, "upstream_chunks": 8},
		Resilience:  make(map[string]bool),
	}
	for _, mode := range []struct {
		name   string
		stream bool
	}{{"nonstream", false}, {"stream", true}} {
		directResult := run(client, "direct", *direct, "", mode.name, mode.stream, *requests, *concurrency, "")
		gatewayResult := run(client, "gateway", *gateway, *apiKey, mode.name, mode.stream, *requests, *concurrency, *metricsURL)
		r.Results = append(r.Results, directResult, gatewayResult)
		r.Overhead = append(r.Overhead, comparePaths(mode.name, directResult, gatewayResult))
	}
	r.Resilience["slow_client_completes"] = slowClient(client, *gateway, *apiKey)
	r.Resilience["client_disconnect_cancels"] = disconnect(client, *gateway, *apiKey)
	r.Resilience["mid_stream_failure_detected"] = midStreamFailure(client, *gateway, *apiKey)

	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
	if *output != "" {
		if err := os.WriteFile(*output, append(out, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write report:", err)
			os.Exit(1)
		}
	}
	for _, ok := range r.Resilience {
		if !ok {
			os.Exit(1)
		}
	}
}

func run(client *http.Client, path, url, key, mode string, stream bool, count, concurrency int, metricsURL string) result {
	jobs := make(chan struct{})
	samples := make(chan sample, count)
	var wg sync.WaitGroup
	var active, peak atomic.Int64
	monitor := startGatewayMonitor(client, metricsURL)
	start := time.Now()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				current := active.Add(1)
				updatePeak(&peak, current)
				samples <- requestOnce(context.Background(), client, url, key, stream, "benchmark")
				active.Add(-1)
			}
		}()
	}
	for i := 0; i < count; i++ {
		jobs <- struct{}{}
	}
	close(jobs)
	wg.Wait()
	close(samples)
	elapsed := time.Since(start)
	all := make([]sample, 0, count)
	for s := range samples {
		all = append(all, s)
	}
	result := summarize(path, mode, all, elapsed)
	result.PeakActive = peak.Load()
	if stream {
		result.PeakActiveStreams = result.PeakActive
	}
	result.GatewayResources = monitor.stop()
	return result
}

func requestOnce(ctx context.Context, client *http.Client, url, key string, stream bool, prompt string) sample {
	body, _ := json.Marshal(map[string]any{"model": "benchmark-model", "stream": stream, "messages": []map[string]string{{"role": "user", "content": prompt}}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return sample{Duration: time.Since(start)}
	}
	defer resp.Body.Close()
	buf := make([]byte, 1)
	n, firstErr := resp.Body.Read(buf)
	ttfb := time.Since(start)
	rest, readErr := io.ReadAll(resp.Body)
	buf = append(buf[:n], rest...)
	ok := resp.StatusCode/100 == 2 && (firstErr == nil || firstErr == io.EOF) && readErr == nil
	if stream {
		ok = ok && bytes.Contains(buf, []byte("[DONE]"))
	}
	return sample{Duration: time.Since(start), TTFB: ttfb, Bytes: int64(len(buf)), OK: ok}
}

func summarize(path, mode string, samples []sample, elapsed time.Duration) result {
	durations, ttfbs := make([]time.Duration, 0, len(samples)), make([]time.Duration, 0, len(samples))
	errors := 0
	var totalBytes int64
	for _, s := range samples {
		durations = append(durations, s.Duration)
		ttfbs = append(ttfbs, s.TTFB)
		totalBytes += s.Bytes
		if !s.OK {
			errors++
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	sort.Slice(ttfbs, func(i, j int) bool { return ttfbs[i] < ttfbs[j] })
	count := float64(len(samples))
	if count == 0 {
		return result{Path: path, Mode: mode}
	}
	return result{Path: path, Mode: mode, Requests: len(samples), Errors: errors, ErrorRate: round(float64(errors)/count, 6), RPS: round(count/elapsed.Seconds(), 3), P50MS: ms(percentile(durations, .50)), P95MS: ms(percentile(durations, .95)), P99MS: ms(percentile(durations, .99)), TTFBP50MS: ms(percentile(ttfbs, .50)), TTFBP95MS: ms(percentile(ttfbs, .95)), TTFBP99MS: ms(percentile(ttfbs, .99)), MeanBytes: round(float64(totalBytes)/count, 3)}
}

func comparePaths(mode string, direct, gateway result) overhead {
	throughputPercent := 0.0
	if direct.RPS != 0 {
		throughputPercent = 100 * (gateway.RPS - direct.RPS) / direct.RPS
	}
	return overhead{
		Mode:              mode,
		ThroughputPercent: round(throughputPercent, 3),
		LatencyP50MS:      round(gateway.P50MS-direct.P50MS, 3),
		LatencyP95MS:      round(gateway.P95MS-direct.P95MS, 3),
		LatencyP99MS:      round(gateway.P99MS-direct.P99MS, 3),
		TTFBP50MS:         round(gateway.TTFBP50MS-direct.TTFBP50MS, 3),
		TTFBP95MS:         round(gateway.TTFBP95MS-direct.TTFBP95MS, 3),
		TTFBP99MS:         round(gateway.TTFBP99MS-direct.TTFBP99MS, 3),
	}
}

func updatePeak(peak *atomic.Int64, value int64) {
	for current := peak.Load(); value > current; current = peak.Load() {
		if peak.CompareAndSwap(current, value) {
			return
		}
	}
}

type metricsSnapshot struct {
	CPUSeconds    float64
	ResidentBytes float64
	HeapBytes     float64
	HasCPU        bool
}

type gatewayMonitor struct {
	client   *http.Client
	url      string
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	firstCPU float64
	lastCPU  float64
	hasCPU   bool
	peakRSS  float64
	peakHeap float64
}

func startGatewayMonitor(client *http.Client, url string) *gatewayMonitor {
	m := &gatewayMonitor{client: client, url: url}
	if url == "" {
		return m
	}
	m.observe(m.fetch())
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	go func() {
		defer close(m.done)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.observe(m.fetch())
			}
		}
	}()
	return m
}

func (m *gatewayMonitor) stop() *gatewayResourceUsage {
	if m.url == "" {
		return nil
	}
	m.cancel()
	<-m.done
	m.observe(m.fetch())
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasCPU && m.peakRSS == 0 && m.peakHeap == 0 {
		return nil
	}
	return &gatewayResourceUsage{
		CPUSeconds:        round(max(0, m.lastCPU-m.firstCPU), 3),
		PeakResidentBytes: m.peakRSS,
		PeakHeapBytes:     m.peakHeap,
	}
}

func (m *gatewayMonitor) observe(snapshot metricsSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if snapshot.HasCPU {
		if !m.hasCPU {
			m.firstCPU = snapshot.CPUSeconds
			m.hasCPU = true
		}
		m.lastCPU = snapshot.CPUSeconds
	}
	m.peakRSS = max(m.peakRSS, snapshot.ResidentBytes)
	m.peakHeap = max(m.peakHeap, snapshot.HeapBytes)
}

func (m *gatewayMonitor) fetch() metricsSnapshot {
	resp, err := m.client.Get(m.url)
	if err != nil {
		return metricsSnapshot{}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return metricsSnapshot{}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return metricsSnapshot{}
	}
	var snapshot metricsSnapshot
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "process_cpu_seconds_total":
			snapshot.CPUSeconds = value
			snapshot.HasCPU = true
		case "process_resident_memory_bytes":
			snapshot.ResidentBytes = value
		case "go_memstats_alloc_bytes":
			snapshot.HeapBytes = value
		}
	}
	return snapshot
}

func slowClient(client *http.Client, url, key string) bool {
	body := strings.NewReader(`{"model":"benchmark-model","stream":true,"messages":[{"role":"user","content":"slow-client"}]}`)
	req, _ := http.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	buf := make([]byte, 32)
	var out bytes.Buffer
	for {
		n, err := resp.Body.Read(buf)
		out.Write(buf[:n])
		time.Sleep(5 * time.Millisecond)
		if err == io.EOF {
			break
		}
		if err != nil {
			return false
		}
	}
	return bytes.Contains(out.Bytes(), []byte("[DONE]"))
}

func disconnect(client *http.Client, url, key string) bool {
	ctx, cancel := context.WithCancel(context.Background())
	body := strings.NewReader(`{"model":"benchmark-model","stream":true,"messages":[{"role":"user","content":"disconnect"}]}`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return false
	}
	buf := make([]byte, 1)
	_, err = resp.Body.Read(buf)
	cancel()
	_ = resp.Body.Close()
	return err == nil
}

func midStreamFailure(client *http.Client, url, key string) bool {
	s := requestOnce(context.Background(), client, url, key, true, "mid-stream-failure")
	return !s.OK && s.Bytes > 0
}

func percentile(values []time.Duration, q float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	i := int(float64(len(values)-1) * q)
	return values[i]
}
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
func round(value float64, places int) float64 {
	factor := math.Pow10(places)
	return math.Round(value*factor) / factor
}
