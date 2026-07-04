package usage

import (
	"context"
	"sync"
	"testing"
	"time"
)


// **回归（review MED#10）**：Close 与并发 Publish 竞态不能 panic——
// 旧实现 close(queue) 时并发 Publish 的 select 可能选中往已关 channel 发送。
// 现在 queue 永不 close，-race 下并发轰炸验证。
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
	// 在 Publish 洪峰中途 Close
	time.Sleep(2 * time.Millisecond)
	_ = o.Close()
	wg.Wait()
	// Close 之后 Publish 拒绝
	if err := o.Publish(context.Background(), &OutboxEvent{Key: "k", Payload: []byte("v")}); err == nil {
		t.Error("Close 后 Publish 应返错")
	}
}

// FileOutbox Close 与并发 Publish 竞态不能 nil deref。
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
		t.Error("Close 后 Publish 应返错")
	}
}
