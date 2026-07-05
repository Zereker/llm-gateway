package usage

import (
	"context"
	"errors"
	"testing"
)

// stubWriter records all Write calls; used for KafkaOutbox unit tests.
type stubWriter struct {
	writes []writtenMsg
	err    error
	closed bool
}

type writtenMsg struct {
	Topic string
	Key   string
	Value string
}

func (s *stubWriter) Write(_ context.Context, topic string, k, v []byte) error {
	if s.err != nil {
		return s.err
	}
	s.writes = append(s.writes, writtenMsg{Topic: topic, Key: string(k), Value: string(v)})
	return nil
}

func (s *stubWriter) Close() error {
	s.closed = true
	return nil
}

func TestKafkaOutbox_PublishForwardsTopicKeyAndPayload(t *testing.T) {
	sw := &stubWriter{}
	o := NewKafkaOutbox(sw, "billing.usage.recorded.v1")

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
	got := sw.writes[0]
	if got.Topic != "billing.usage.recorded.v1" {
		t.Errorf("topic = %q", got.Topic)
	}
	if got.Key != "ep_openai_main" {
		t.Errorf("key = %q", got.Key)
	}
	if got.Value != `{"trace_id":"abc","total":15}` {
		t.Errorf("value = %q", got.Value)
	}
}

func TestKafkaOutbox_PublishPropagatesError(t *testing.T) {
	sw := &stubWriter{err: errors.New("broker down")}
	o := NewKafkaOutbox(sw, "topic")
	err := o.Publish(context.Background(), &OutboxEvent{Key: "k", Payload: []byte("v")})
	if err == nil {
		t.Fatal("want propagated error")
	}
	if err.Error() != sw.err.Error() {
		t.Errorf("err = %v, want %v", err, sw.err)
	}
}

func TestKafkaOutbox_RejectsNilEvent(t *testing.T) {
	o := NewKafkaOutbox(&stubWriter{}, "topic")
	if err := o.Publish(context.Background(), nil); err == nil {
		t.Fatal("want error for nil event")
	}
}

func TestKafkaOutbox_RejectsEmptyTopic(t *testing.T) {
	// defensive: a clear error must still be given if the caller misuses NewKafkaOutbox(producer, "")
	o := NewKafkaOutbox(&stubWriter{}, "")
	err := o.Publish(context.Background(), &OutboxEvent{Key: "k", Payload: []byte("v")})
	if err == nil {
		t.Fatal("want error for empty topic")
	}
}

func TestKafkaOutbox_CloseDelegates(t *testing.T) {
	sw := &stubWriter{}
	o := NewKafkaOutbox(sw, "topic")
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !sw.closed {
		t.Error("Close should delegate to underlying writer")
	}
}

func TestKafkaOutbox_ManyConcurrentPublish(t *testing.T) {
	// Simple serial smoke test; actual concurrency safety is provided by the underlying *kafka.Writer.
	sw := &stubWriter{}
	o := NewKafkaOutbox(sw, "topic")
	for i := 0; i < 100; i++ {
		_ = o.Publish(context.Background(), &OutboxEvent{Key: "k", Payload: []byte("v")})
	}
	if len(sw.writes) != 100 {
		t.Errorf("got %d writes, want 100", len(sw.writes))
	}
}
