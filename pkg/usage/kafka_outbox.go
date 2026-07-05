package usage

import (
	"context"
	"errors"
)

// KafkaWriter is KafkaOutbox's dependency on the underlying Kafka producer.
//
// Accepts any object implementing "synchronous write by (topic, key, value)
// + close" semantics:
//   - default: *infra.KafkaProducer (topic-agnostic, specified on each Write)
//   - tests: stub in this package
//   - user-defined: can wrap instrumentation / rate-limiting
//
// This keeps pkg/usage from directly depending on pkg/infra — loose coupling + testable.
type KafkaWriter interface {
	Write(c context.Context, topic string, key, value []byte) error
	Close() error
}

// KafkaOutbox sends an OutboxEvent to the specified topic via a Kafka producer.
//
// The topic is bound at construction time (each outbox instance corresponds
// to one business-purpose topic); the same KafkaProducer can be shared by
// multiple outboxes (usage / audit / ...).
//
// Implements OutboxPublisher + io.Closer.
type KafkaOutbox struct {
	w     KafkaWriter
	topic string
}

// NewKafkaOutbox constructs from a ready-made KafkaWriter + topic.
//
// Typical usage: usage.NewKafkaOutbox(infra.NewKafkaProducer(brokers), "billing.usage.recorded.v1")
func NewKafkaOutbox(w KafkaWriter, topic string) *KafkaOutbox {
	return &KafkaOutbox{w: w, topic: topic}
}

// Publish implements OutboxPublisher.Publish.
//
// evt.Payload is used directly as the Kafka message value (already
// serialized JSON / Protobuf); evt.Key is used as the partition key
// (defaults to EndpointID, so events from the same endpoint land on the
// same partition).
func (o *KafkaOutbox) Publish(ctx context.Context, evt *OutboxEvent) error {
	if evt == nil {
		return errors.New("usage: KafkaOutbox.Publish: nil event")
	}
	if o.topic == "" {
		return errors.New("usage: KafkaOutbox: empty topic (must be set at NewKafkaOutbox)")
	}
	return o.w.Write(ctx, o.topic, []byte(evt.Key), evt.Payload)
}

// Close closes the underlying producer.
//
// Note: if a KafkaProducer is shared by multiple outboxes, the caller must
// coordinate who is responsible for Close (to avoid closing it twice) —
// typically main holds the producer reference directly and closes it centrally.
func (o *KafkaOutbox) Close() error {
	return o.w.Close()
}

// Compile-time assertion.
var _ OutboxPublisher = (*KafkaOutbox)(nil)
