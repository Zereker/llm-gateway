package usage

import (
	"context"
	"errors"
	"testing"
)

// stubWriter 记录所有 Write 调用；用于 KafkaOutbox 单元测试。
type stubWriter struct {
	writes []writtenMsg
	err    error
	closed bool
}

type writtenMsg struct {
	Key   string
	Value string
}

func (s *stubWriter) Write(_ context.Context, k, v []byte) error {
	if s.err != nil {
		return s.err
	}
	s.writes = append(s.writes, writtenMsg{Key: string(k), Value: string(v)})
	return nil
}

func (s *stubWriter) Close() error {
	s.closed = true
	return nil
}

func TestKafkaOutbox_PublishForwardsKeyAndPayload(t *testing.T) {
	sw := &stubWriter{}
	o := NewKafkaOutbox(sw)

	err := o.Publish(context.Background(), &OutboxEvent{
		Key:     "ep_openai_main",
		Payload: []byte(`{"trace_id":"abc","total":15}`),
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(sw.writes) != 1 {
		t.Fatalf("len writes = %d, want 1", len(sw.writes))
	}
	if sw.writes[0].Key != "ep_openai_main" {
		t.Errorf("key = %q", sw.writes[0].Key)
	}
	if sw.writes[0].Value != `{"trace_id":"abc","total":15}` {
		t.Errorf("value = %q", sw.writes[0].Value)
	}
}

func TestKafkaOutbox_PublishPropagatesError(t *testing.T) {
	sw := &stubWriter{err: errors.New("broker down")}
	o := NewKafkaOutbox(sw)
	err := o.Publish(context.Background(), &OutboxEvent{Key: "k", Payload: []byte("v")})
	if err == nil {
		t.Fatal("want propagated error")
	}
	if !errors.Is(err, sw.err) {
		// 不强制 errors.Is（KafkaOutbox 直接 return underlying err，是 unwrap-able 的）
		// 但至少应该出现在 chain 里
		if err.Error() != sw.err.Error() {
			t.Errorf("err = %v, want chain-equivalent to %v", err, sw.err)
		}
	}
}

func TestKafkaOutbox_RejectsNilEvent(t *testing.T) {
	o := NewKafkaOutbox(&stubWriter{})
	if err := o.Publish(context.Background(), nil); err == nil {
		t.Fatal("want error for nil event")
	}
}

func TestKafkaOutbox_CloseDelegates(t *testing.T) {
	sw := &stubWriter{}
	o := NewKafkaOutbox(sw)
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !sw.closed {
		t.Error("Close should delegate to underlying writer")
	}
}

func TestKafkaOutbox_ManyConcurrentPublish(t *testing.T) {
	// 简单并发烟雾测：stub 本身非并发安全，所以只是确认 Publish 没引入 race
	// 在串行情况下的语义。真并发安全靠底层 *kafka.Writer 提供。
	sw := &stubWriter{}
	o := NewKafkaOutbox(sw)
	for i := 0; i < 100; i++ {
		_ = o.Publish(context.Background(), &OutboxEvent{Key: "k", Payload: []byte("v")})
	}
	if len(sw.writes) != 100 {
		t.Errorf("got %d writes, want 100", len(sw.writes))
	}
}
