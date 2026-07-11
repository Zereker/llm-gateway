package usage

import (
	"context"
	"sync"
	"testing"
	"time"
)


// **Regression (review MED#10)**: a race between Close and concurrent
// Publish must not panic — in the old implementation, close(queue) meant a
// concurrent Publish's select could pick sending on an already-closed
// channel. Now queue is never closed; verified by concurrent bombardment under -race.
func TestAsyncKafkaOutbox_ConcurrentPublishCloseNoPanic(t *testing.T) {
	w := &stubWriter{}
	o := NewAsyncKafkaOutbox(w, "t", AsyncOptions{BufferSize: 8})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
				_ = o.Publish(ctx, &OutboxEvent{Key: "k", Payload: []byte("v")})
				cancel()
			}
		}()
	}
	// Close midway through the Publish flood
	time.Sleep(2 * time.Millisecond)
	_ = o.Close()
	wg.Wait()
	// Publish should be rejected after Close
	if err := o.Publish(context.Background(), &OutboxEvent{Key: "k", Payload: []byte("v")}); err == nil {
		t.Error("Publish after Close should return an error")
	}
}

// A race between Close and concurrent Publish in FileOutbox must not nil deref.
func TestFileOutbox_ConcurrentPublishCloseNoPanic(t *testing.T) {
	f, err := NewFileOutbox(t.TempDir() + "/usage.jsonl")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = f.Publish(context.Background(), &OutboxEvent{Payload: []byte("x")})
			}
		}()
	}
	time.Sleep(time.Millisecond)
	_ = f.Close()
	wg.Wait()
	if err := f.Publish(context.Background(), &OutboxEvent{Payload: []byte("x")}); err == nil {
		t.Error("Publish after Close should return an error")
	}
}
