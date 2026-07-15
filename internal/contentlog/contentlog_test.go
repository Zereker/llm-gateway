package contentlog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/internal/domain"
)

type collectingPublisher struct {
	mu      sync.Mutex
	records []*Record
}

func (p *collectingPublisher) snapshot() []*Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*Record(nil), p.records...)
}

func (p *collectingPublisher) Publish(_ context.Context, r *Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records = append(p.records, r)
	return nil
}

func TestLoggerTruncatesEnrichesAndClosesIdempotently(t *testing.T) {
	pub := &collectingPublisher{}
	l := New(Config{Publisher: pub, SampleRate: 1, MaxBodyBytes: 3, BufferSize: 2})
	ctx := EnrichCtx(context.Background(), RequestEnrich{RequestID: "req-1", AccountID: "acct"})
	l.OnClientRequest(ctx, &domain.Endpoint{ID: 7, Vendor: "openai"}, []byte("abcdef"))

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := l.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(closeCtx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if len(pub.records) != 1 {
		t.Fatalf("records = %d", len(pub.records))
	}
	r := pub.records[0]
	if string(r.Body) != "abc" || !r.Truncated || r.RequestID != "req-1" || r.EndpointID != "7" {
		t.Fatalf("record = %+v", r)
	}
}

type failingRedactor struct{}

func (failingRedactor) Redact(Direction, []byte) ([]byte, bool, error) {
	return nil, false, context.Canceled
}

func TestLoggerDropsWhenRedactionFails(t *testing.T) {
	pub := &collectingPublisher{}
	l := New(Config{Publisher: pub, Redactor: failingRedactor{}, SampleRate: 1})
	l.OnClientRequest(context.Background(), nil, []byte("secret"))
	if l.Dropped() != 1 {
		t.Fatalf("dropped = %d", l.Dropped())
	}
	_ = l.Close(context.Background())
	if len(pub.records) != 0 {
		t.Fatal("unredacted record was published")
	}
}

type redactorFunc func(Direction, []byte) ([]byte, bool, error)

func (f redactorFunc) Redact(direction Direction, body []byte) ([]byte, bool, error) {
	return f(direction, body)
}

func TestLoggerObserverDirectionsRedactionAndEnrichment(t *testing.T) {
	pub := &collectingPublisher{}
	l := New(Config{
		Publisher: pub, SampleRate: 2, BufferSize: 8,
		Redactor: redactorFunc(func(_ Direction, body []byte) ([]byte, bool, error) {
			return []byte(strings.ReplaceAll(string(body), "secret", "[MASKED]")), true, nil
		}),
	})
	if l.cfg.SampleRate != 1 {
		t.Fatalf("sample rate = %v", l.cfg.SampleRate)
	}
	ctx := EnrichCtx(context.Background(), RequestEnrich{
		RequestID: "req", TraceID: "trace", AccountID: "acct", APIKeyID: "key",
		SubAccountID: "sub", Model: "model", Protocol: "openai", Modality: "chat",
	})
	ep := &domain.Endpoint{ID: -7, Vendor: "vendor"}
	body := []byte("secret")
	l.OnClientRequest(ctx, ep, body)
	body[0] = 'X'
	l.OnUpstreamRequest(ctx, ep, []byte("secret"))
	l.OnUpstreamChunk(ctx, ep, []byte("secret"))
	l.OnClientChunk(ctx, ep, []byte("secret"))
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	records := pub.snapshot()
	if len(records) != 4 {
		t.Fatalf("records = %d", len(records))
	}
	directions := []Direction{DirClientRequest, DirUpstreamRequest, DirUpstreamChunk, DirClientChunk}
	wantHash := sha256.Sum256([]byte("[MASKED]"))
	for i, record := range records {
		if record.Direction != directions[i] || string(record.Body) != "[MASKED]" || !record.Redacted {
			t.Fatalf("record[%d] = %+v", i, record)
		}
		if record.BodySHA256 != hex.EncodeToString(wantHash[:]) || record.EndpointID != "-7" || record.Vendor != "vendor" {
			t.Fatalf("record[%d] hash/endpoint = %+v", i, record)
		}
		if record.RequestID != "req" || record.TraceID != "trace" || record.AccountID != "acct" ||
			record.APIKeyID != "key" || record.SubAccountID != "sub" || record.Model != "model" ||
			record.Protocol != "openai" || record.Modality != "chat" || record.CreatedAt.IsZero() {
			t.Fatalf("record[%d] enrichment = %+v", i, record)
		}
	}
}

func TestLoggerSamplingDisabledNilAndClosedAreNoops(t *testing.T) {
	var nilLogger *Logger
	nilLogger.OnClientRequest(context.Background(), nil, []byte("ignored"))

	pub := &collectingPublisher{}
	l := New(Config{Publisher: pub, SampleRate: 0})
	l.OnClientRequest(context.Background(), nil, []byte("sampled out"))
	if l.shouldSample() {
		t.Fatal("zero sample rate sampled in")
	}
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.OnClientRequest(context.Background(), nil, []byte("closed"))
	if len(pub.snapshot()) != 0 || l.Dropped() != 0 {
		t.Fatalf("records=%d dropped=%d", len(pub.snapshot()), l.Dropped())
	}

	withoutPublisher := New(Config{SampleRate: 1})
	withoutPublisher.OnClientRequest(context.Background(), nil, []byte("ignored"))
	_ = withoutPublisher.Close(context.Background())
	var nilContext context.Context
	enrichFromCtx(nilContext, &Record{})

	partial := New(Config{Publisher: pub, SampleRate: 0.5})
	_ = partial.shouldSample()
	_ = partial.Close(context.Background())
}

type blockingPublisher struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	bodies  []string
}

func newBlockingPublisher() *blockingPublisher {
	return &blockingPublisher{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *blockingPublisher) Publish(_ context.Context, record *Record) error {
	p.once.Do(func() { close(p.started) })
	<-p.release
	p.mu.Lock()
	p.bodies = append(p.bodies, string(record.Body))
	p.mu.Unlock()
	return nil
}

func (p *blockingPublisher) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.bodies...)
}

func TestLoggerBackpressureStrategies(t *testing.T) {
	tests := []struct {
		name       string
		strategy   BackpressureStrategy
		reason     string
		wantBodies []string
	}{
		{"drop newest", BackpressureDropNewest, "buffer_full", []string{"first", "second"}},
		{"drop oldest", BackpressureDropOldest, "drop_oldest", []string{"first", "third"}},
		{"block timeout", BackpressureBlock, "block_timeout", []string{"first", "second"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pub := newBlockingPublisher()
			var reasons []string
			l := New(Config{
				Publisher: pub, SampleRate: 1, BufferSize: 1,
				Backpressure: tc.strategy, BlockTimeout: 5 * time.Millisecond,
				OnDrop: func(reason string) { reasons = append(reasons, reason) },
			})
			l.OnClientRequest(context.Background(), nil, []byte("first"))
			select {
			case <-pub.started:
			case <-time.After(time.Second):
				t.Fatal("publisher did not start")
			}
			l.OnClientRequest(context.Background(), nil, []byte("second"))
			l.OnClientRequest(context.Background(), nil, []byte("third"))
			if l.Dropped() != 1 || len(reasons) != 1 || reasons[0] != tc.reason {
				t.Fatalf("dropped=%d reasons=%v", l.Dropped(), reasons)
			}
			close(pub.release)
			if err := l.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := pub.snapshot()
			if strings.Join(got, ",") != strings.Join(tc.wantBodies, ",") {
				t.Fatalf("bodies=%v want=%v", got, tc.wantBodies)
			}
		})
	}
}

func TestLoggerCloseHonorsContextWhilePublisherBlocked(t *testing.T) {
	pub := newBlockingPublisher()
	l := New(Config{Publisher: pub, SampleRate: 1, BufferSize: 1})
	l.OnClientRequest(context.Background(), nil, []byte("body"))
	<-pub.started

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := l.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close err=%v", err)
	}
	close(pub.release)
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type failingPublisher struct{ err error }

func (p failingPublisher) Publish(context.Context, *Record) error { return p.err }

func TestLoggerPublishFailureDoesNotAffectClose(t *testing.T) {
	l := New(Config{Publisher: failingPublisher{err: errors.New("sink down")}, SampleRate: 1})
	l.publish(nil)
	l.OnClientRequest(context.Background(), nil, []byte("body"))
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.Dropped() != 0 {
		t.Fatalf("publish failures are not queue drops: %d", l.Dropped())
	}
}

func TestLoggerDefaultsAndFormatInt(t *testing.T) {
	l := New(Config{Publisher: &collectingPublisher{}, SampleRate: 1, Backpressure: BackpressureBlock})
	if cap(l.queue) != 1024 || l.cfg.BlockTimeout != 50*time.Millisecond {
		t.Fatalf("queue=%d timeout=%s", cap(l.queue), l.cfg.BlockTimeout)
	}
	_ = l.Close(context.Background())
	for value, want := range map[int64]string{0: "", 7: "7", -42: "-42"} {
		if got := formatInt(value); got != want {
			t.Fatalf("formatInt(%d)=%q want=%q", value, got, want)
		}
	}
}

func TestFilePublisherWritesJSONLAndClosesIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	publisher, err := NewFilePublisher(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), &Record{RequestID: "one", Direction: DirClientRequest}); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), &Record{RequestID: "two", Direction: DirClientChunk}); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"request_id":"one"`) || !strings.Contains(lines[1], `"request_id":"two"`) {
		t.Fatalf("jsonl=%s", raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("content log permissions = %o, want 600", got)
	}

	if _, err := NewFilePublisher(filepath.Join(t.TempDir(), "missing", "content.jsonl")); err == nil {
		t.Fatal("opening a file below a missing directory succeeded")
	}
}
