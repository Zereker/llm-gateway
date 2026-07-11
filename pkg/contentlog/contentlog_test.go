package contentlog

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
)

type collectingPublisher struct {
	mu      sync.Mutex
	records []*Record
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
