package usage

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// samplePayload simulates a typical UsageEvent JSON marshaled by M10 Tracing —
// includes the envelope + nested Usage / Meta fields, roughly 400-500 B long.
var samplePayload = []byte(`{"schema_version":"usage.v1","event_id":"01JABCDEF1234567890ABCDEF","request_id":"req_abc123def456","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","usage":{"input":128,"output":256,"total":384,"source":"upstream","confidence":"exact","meta":{"account_id":"acct_alice","sub_account_id":"team1","api_key_id":"key_xyz","model":"gpt-4o","vendor":"openai","endpoint_id":"ep_openai_main","start_time":"2026-05-17T10:00:00Z","end_time":"2026-05-17T10:00:02Z"}},"created_at":"2026-05-17T10:00:02Z"}`)

// =============================================================================
// Concurrency correctness: does removing the explicit mutex still guarantee JSONL lines don't interleave
// =============================================================================

// TestFileOutbox_ConcurrentPublishDoesNotInterleave verifies that *os.File.Write's
// internal fdmutex guarantees JSONL line atomicity under a single Write call,
// so even with multiple goroutines concurrently publishing, a line never
// gets split in half (writes 4000 lines, then scans line-by-line with
// bufio.Scanner — every line must deserialize into the complete payload).
func TestFileOutbox_ConcurrentPublishDoesNotInterleave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "u.log")
	o, err := NewFileOutbox(path)
	if err != nil {
		t.Fatalf("NewFileOutbox: %v", err)
	}

	const (
		goroutines       = 16
		publishPerWorker = 250
		expectedTotal    = goroutines * publishPerWorker
	)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for w := 0; w < goroutines; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < publishPerWorker; i++ {
				_ = o.Publish(context.Background(), &OutboxEvent{
					Key:     "k",
					Payload: samplePayload,
				})
			}
		}()
	}
	wg.Wait()
	_ = o.Close()

	// Verify: every line equals samplePayload exactly, with no misalignment / truncation / interleaving
	f, _ := os.Open(path)
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	want := string(samplePayload)
	lines := 0
	for scanner.Scan() {
		lines++
		got := scanner.Text()
		if got != want {
			t.Fatalf("line %d corrupted: got %q (len=%d), want %q (len=%d)",
				lines, got, len(got), want, len(want))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}
	if lines != expectedTotal {
		t.Errorf("got %d lines, want %d", lines, expectedTotal)
	}
}

// =============================================================================
// Benchmark: FileOutbox's current implementation (optimized)
// =============================================================================

func BenchmarkFileOutbox_Publish(b *testing.B) {
	dir := b.TempDir()
	o, _ := NewFileOutbox(filepath.Join(dir, "u.log"))
	defer o.Close()

	evt := &OutboxEvent{Key: "acct1", Payload: samplePayload}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := o.Publish(context.Background(), evt); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFileOutbox_PublishParallel(b *testing.B) {
	dir := b.TempDir()
	o, _ := NewFileOutbox(filepath.Join(dir, "u.log"))
	defer o.Close()

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		evt := &OutboxEvent{Key: "k", Payload: samplePayload}
		for pb.Next() {
			if err := o.Publish(context.Background(), evt); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// =============================================================================
// Benchmark: slog.JSONHandler as a control (writes to the same file, similar payload fields)
// =============================================================================

// BenchmarkSlog_JSONFile uses slog to write equivalent JSON to a file as a control.
//
// Note: slog cannot fully replace FileOutbox (errors are swallowed + it
// forces a time/level/msg wrapper) — this is only for performance
// comparison; see docs/05 §5 + the FileOutbox comment for details.
func BenchmarkSlog_JSONFile(b *testing.B) {
	dir := b.TempDir()
	f, _ := os.Create(filepath.Join(dir, "slog.log"))
	defer f.Close()

	logger := slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		logger.Info("usage_event",
			"schema_version", "usage.v1",
			"event_id", "01JABCDEF1234567890ABCDEF",
			"request_id", "req_abc123def456",
			"trace_id", "4bf92f3577b34da6a3ce929d0e0e4736",
			"input", 128,
			"output", 256,
			"total", 384,
			"account_id", "acct_alice",
			"model", "gpt-4o",
			"vendor", "openai",
			"endpoint_id", "ep_openai_main",
		)
	}
}

// BenchmarkSlog_JSONFileParallel is the concurrent version of slog (slog's handler internally serializes Write with a mutex)
func BenchmarkSlog_JSONFileParallel(b *testing.B) {
	dir := b.TempDir()
	f, _ := os.Create(filepath.Join(dir, "slog.log"))
	defer f.Close()

	logger := slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			logger.Info("usage_event",
				"event_id", "01JABCDEF1234567890ABCDEF",
				"request_id", "req_abc123def456",
				"total", 384,
				"account_id", "acct_alice",
				"model", "gpt-4o",
			)
		}
	})
}
