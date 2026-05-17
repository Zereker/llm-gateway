package usage

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// samplePayload 模拟 M10 Tracing marshal 出来的典型 UsageEvent JSON——
// 包含 envelope + 嵌套 Usage / Meta 字段，长度约 400-500 B。
var samplePayload = []byte(`{"schema_version":"usage.v1","event_id":"01JABCDEF1234567890ABCDEF","request_id":"req_abc123def456","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","usage":{"input":128,"output":256,"total":384,"source":"upstream","confidence":"exact","meta":{"account_id":"acct_alice","sub_account_id":"team1","api_key_id":"key_xyz","model":"gpt-4o","vendor":"openai","endpoint_id":"ep_openai_main","start_time":"2026-05-17T10:00:00Z","end_time":"2026-05-17T10:00:02Z"}},"created_at":"2026-05-17T10:00:02Z"}`)

// =============================================================================
// 并发正确性：去掉显式 mutex 后是否仍保证 JSONL 行不交错
// =============================================================================

// TestFileOutbox_ConcurrentPublishDoesNotInterleave 验证 *os.File.Write 内部
// fdmutex 在单次 Write 调用下保证 JSONL 行原子，即使多 goroutine 并发也不会
// 把一行劈成两半（写 4000 行后用 bufio.Scanner 按行扫，每行必须能反序列化为
// 完整 payload）。
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

	// 验证：每行原样等于 samplePayload，没有错位 / 截断 / 交错
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
// Benchmark: FileOutbox 当前实现（优化后）
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
// Benchmark: slog.JSONHandler 作为对照（写同一个 file，相似 payload 字段）
// =============================================================================

// BenchmarkSlog_JSONFile 用 slog 写等价的 JSON 到文件做对照。
//
// 注意：slog 不能完全替代 FileOutbox（error 被吞 + 强制 time/level/msg 包装），
// 这里只为性能对比；详见 docs/05 §5 + FileOutbox 注释。
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

// BenchmarkSlog_JSONFileParallel slog 并发版本（slog handler 内部用 mutex 串行化 Write）
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

// =============================================================================
// Benchmark: DualWriteOutbox（file + 空 Kafka 桩）
// =============================================================================

// noopKafka 模拟 AsyncKafkaOutbox.Publish 的"入队即返回"语义——实际的
// AsyncKafkaOutbox 是 channel send，跟 noop 在 hot path 上开销同量级。
type noopKafka struct{}

func (noopKafka) Publish(_ context.Context, _ *OutboxEvent) error { return nil }
func (noopKafka) Close() error                                    { return nil }

var _ io.Closer = noopKafka{}

func BenchmarkDualWriteOutbox_Publish(b *testing.B) {
	dir := b.TempDir()
	file, _ := NewFileOutbox(filepath.Join(dir, "u.log"))
	defer file.Close()

	dual := NewDualWriteOutbox(file, noopKafka{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	evt := &OutboxEvent{Key: "acct1", Payload: samplePayload}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := dual.Publish(context.Background(), evt); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDualWriteOutbox_PublishParallel(b *testing.B) {
	dir := b.TempDir()
	file, _ := NewFileOutbox(filepath.Join(dir, "u.log"))
	defer file.Close()

	dual := NewDualWriteOutbox(file, noopKafka{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		evt := &OutboxEvent{Key: "k", Payload: samplePayload}
		for pb.Next() {
			if err := dual.Publish(context.Background(), evt); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// 防 unused
var _ = strings.Contains
