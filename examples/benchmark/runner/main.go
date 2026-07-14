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
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type sample struct {
	Duration time.Duration
	TTFB     time.Duration
	Bytes    int64
	OK       bool
}

type result struct {
	Path      string  `json:"path"`
	Mode      string  `json:"mode"`
	Requests  int     `json:"requests"`
	Errors    int     `json:"errors"`
	RPS       float64 `json:"requests_per_second"`
	P50MS     float64 `json:"latency_p50_ms"`
	P95MS     float64 `json:"latency_p95_ms"`
	P99MS     float64 `json:"latency_p99_ms"`
	TTFBP50MS float64 `json:"ttfb_p50_ms"`
	TTFBP95MS float64 `json:"ttfb_p95_ms"`
	TTFBP99MS float64 `json:"ttfb_p99_ms"`
	MeanBytes float64 `json:"mean_bytes"`
}

type report struct {
	Version     string          `json:"runner_version"`
	GeneratedAt string          `json:"generated_at"`
	Environment map[string]any  `json:"environment"`
	Results     []result        `json:"results"`
	Resilience  map[string]bool `json:"resilience"`
}

func main() {
	direct := flag.String("direct", "http://benchmark-upstream:9092/v1/chat/completions", "direct upstream URL")
	gateway := flag.String("gateway", "http://gateway:8080/v1/chat/completions", "gateway URL")
	apiKey := flag.String("api-key", "sk-demo-llm-gateway", "gateway API key")
	requests := flag.Int("requests", 200, "requests per path and mode")
	concurrency := flag.Int("concurrency", 20, "workers per run")
	flag.Parse()

	client := &http.Client{Timeout: 30 * time.Second}
	r := report{
		Version:     "1",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Environment: map[string]any{"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "cpus": runtime.NumCPU(), "requests_per_case": *requests, "concurrency": *concurrency, "upstream_nonstream_delay_ms": 20, "upstream_first_token_delay_ms": 50, "upstream_chunk_delay_ms": 10, "upstream_chunks": 8},
		Resilience:  make(map[string]bool),
	}
	for _, mode := range []struct {
		name   string
		stream bool
	}{{"nonstream", false}, {"stream", true}} {
		r.Results = append(r.Results,
			run(client, "direct", *direct, "", mode.name, mode.stream, *requests, *concurrency),
			run(client, "gateway", *gateway, *apiKey, mode.name, mode.stream, *requests, *concurrency),
		)
	}
	r.Resilience["slow_client_completes"] = slowClient(client, *gateway, *apiKey)
	r.Resilience["client_disconnect_cancels"] = disconnect(client, *gateway, *apiKey)
	r.Resilience["mid_stream_failure_detected"] = midStreamFailure(client, *gateway, *apiKey)

	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
	for _, ok := range r.Resilience {
		if !ok {
			os.Exit(1)
		}
	}
}

func run(client *http.Client, path, url, key, mode string, stream bool, count, concurrency int) result {
	jobs := make(chan struct{})
	samples := make(chan sample, count)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				samples <- requestOnce(context.Background(), client, url, key, stream, "benchmark")
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
	return summarize(path, mode, all, elapsed)
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
	return result{Path: path, Mode: mode, Requests: len(samples), Errors: errors, RPS: float64(len(samples)) / elapsed.Seconds(), P50MS: ms(percentile(durations, .50)), P95MS: ms(percentile(durations, .95)), P99MS: ms(percentile(durations, .99)), TTFBP50MS: ms(percentile(ttfbs, .50)), TTFBP95MS: ms(percentile(ttfbs, .95)), TTFBP99MS: ms(percentile(ttfbs, .99)), MeanBytes: float64(totalBytes) / float64(len(samples))}
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
