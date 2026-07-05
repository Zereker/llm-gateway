package usage

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
)

// stubPublisher is an OutboxPublisher stub for unit tests; records all Publish calls + can inject failures.
type stubPublisher struct {
	mu       sync.Mutex
	events   []*OutboxEvent
	err      error
	closed   bool
	closeErr error
}

func (s *stubPublisher) Publish(_ context.Context, evt *OutboxEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.events = append(s.events, evt)
	return nil
}

func (s *stubPublisher) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.closeErr
}

func (s *stubPublisher) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func (s *stubPublisher) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func TestDualWrite_BothSucceed_ForwardsToBoth(t *testing.T) {
	file := &stubPublisher{}
	kafka := &stubPublisher{}
	o := NewDualWriteOutbox(file, kafka, slog.Default())

	evt := &OutboxEvent{Key: "acct_alice", Payload: []byte(`{"total":100}`)}
	if err := o.Publish(context.Background(), evt); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if file.count() != 1 {
		t.Errorf("file got %d events, want 1", file.count())
	}
	if kafka.count() != 1 {
		t.Errorf("kafka got %d events, want 1", kafka.count())
	}
	if file.events[0].Key != "acct_alice" {
		t.Errorf("file event key = %q, want acct_alice", file.events[0].Key)
	}
}

func TestDualWrite_FileOKKafkaFail_ReturnsNil(t *testing.T) {
	// file is the source of truth; a kafka failure doesn't count as a failure
	// (the data is already committed; the replay tool backfills it later).
	file := &stubPublisher{}
	kafka := &stubPublisher{err: errors.New("broker down")}
	o := NewDualWriteOutbox(file, kafka, slog.Default())

	err := o.Publish(context.Background(), &OutboxEvent{Key: "k", Payload: []byte("v")})
	if err != nil {
		t.Errorf("want nil err (file commit ok), got: %v", err)
	}
	if file.count() != 1 {
		t.Errorf("file should have event, count=%d", file.count())
	}
}

// **Invariant (review MED#10)**: file ⊇ kafka — kafka is **not** sent when
// the file write fails. Otherwise events would show up in kafka that aren't
// in file, and consumer-vs-file reconciliation couldn't distinguish a
// "kafka phantom" from "file data loss", and file would stop being the
// source of truth.
func TestDualWrite_FileFail_ReturnsError_DoesNotPublishKafka(t *testing.T) {
	file := &stubPublisher{err: errors.New("disk full")}
	kafka := &stubPublisher{}
	o := NewDualWriteOutbox(file, kafka, slog.Default())

	err := o.Publish(context.Background(), &OutboxEvent{Key: "k", Payload: []byte("v")})
	if err == nil {
		t.Fatal("want file err")
	}
	if err.Error() != "disk full" {
		t.Errorf("err = %v, want 'disk full'", err)
	}
	if kafka.count() != 0 {
		t.Errorf("kafka must not be published when file fails (file⊇kafka invariant), kafka count=%d", kafka.count())
	}
}

func TestDualWrite_BothFail_ReturnsFileErr(t *testing.T) {
	// On a double failure, the file error is returned (this is the more
	// serious fault — a disk problem; the kafka error is swallowed into a metric).
	file := &stubPublisher{err: errors.New("disk full")}
	kafka := &stubPublisher{err: errors.New("broker down")}
	o := NewDualWriteOutbox(file, kafka, slog.Default())

	err := o.Publish(context.Background(), &OutboxEvent{Key: "k", Payload: []byte("v")})
	if err == nil || err.Error() != "disk full" {
		t.Errorf("want file err 'disk full', got: %v", err)
	}
}

func TestDualWrite_RejectsNilEvent(t *testing.T) {
	o := NewDualWriteOutbox(&stubPublisher{}, &stubPublisher{}, slog.Default())
	if err := o.Publish(context.Background(), nil); err == nil {
		t.Fatal("want error for nil event")
	}
}

func TestDualWrite_CloseClosesFileOnly(t *testing.T) {
	// the kafka producer's lifecycle is managed by srv, not closed within DualWriteOutbox.Close.
	file := &stubPublisher{}
	kafka := &stubPublisher{}
	o := NewDualWriteOutbox(file, kafka, slog.Default())

	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !file.isClosed() {
		t.Error("file should be closed")
	}
	if kafka.isClosed() {
		t.Error("kafka should NOT be closed (managed by srv, avoids double-close)")
	}
}

func TestDualWrite_NilLoggerUsesDefault(t *testing.T) {
	file := &stubPublisher{}
	kafka := &stubPublisher{}
	o := NewDualWriteOutbox(file, kafka, nil) // nil logger fallback to slog.Default()

	if err := o.Publish(context.Background(), &OutboxEvent{Key: "k", Payload: []byte("v")}); err != nil {
		t.Fatalf("Publish with nil logger: %v", err)
	}
}

func TestDualWrite_ConcurrentPublishesAllReachBothSinks(t *testing.T) {
	file := &stubPublisher{}
	kafka := &stubPublisher{}
	o := NewDualWriteOutbox(file, kafka, slog.Default())

	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_ = o.Publish(context.Background(), &OutboxEvent{Key: "k", Payload: []byte("v")})
		}()
	}
	wg.Wait()

	if file.count() != N {
		t.Errorf("file got %d, want %d", file.count(), N)
	}
	if kafka.count() != N {
		t.Errorf("kafka got %d, want %d", kafka.count(), N)
	}
}
