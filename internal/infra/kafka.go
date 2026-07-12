package infra

import (
	"context"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
)

// KafkaConfig is the Kafka producer connection configuration.
//
// internal/config exposes these fields to yaml by referencing this type;
// concretely it shows up at locations like outbox.kafka.brokers. Future
// extensions (TLS / SASL / ClientID / etc.) only add fields to this type,
// without touching the internal/config struct.
type KafkaConfig struct {
	Brokers []string `yaml:"brokers"`
	// Future: TLS, SASL, ClientID, ...
}

// KafkaProducer is a thin wrapper around a Kafka producer; it is
// topic-agnostic, and the caller specifies the topic per message.
//
// Design trade-offs (v0.1):
//   - **Synchronous** (Async=false): each Write waits for the broker
//     leader's ack and returns the error immediately; performance is
//     sufficient (M10 runs after the response has already been written,
//     so it doesn't block user-perceived latency), and it's easier to debug
//   - **ack=1** (RequireOne): waits only for the leader's confirmation, not
//     all replicas; a durability-vs-latency trade-off
//   - **LeastBytes balancer**: picks a partition by its current queue byte
//     count, giving an even distribution; evt.Key can be used as the
//     partition key (kafka-go prefers hashing by key)
//   - **No idempotent producer / EOS**: downstream consumers are
//     responsible for dedup
//   - **Topic is not bound to the producer**: one producer can send to any
//     topic; each business caller (usage outbox / future audit outbox /
//     ...) holds its own topic string
//
// The caller is responsible for Close (typically in the cleanup chain of
// cmd/X/main.go).
type KafkaProducer struct {
	w *kafka.Writer
}

// NewKafkaProducer constructs a synchronous producer connected to cfg.Brokers.
//
// The topic is not bound here — the caller specifies it on each Write.
// No ping is performed (kafka.Writer is a lazy connect); the connection is
// actually established on the first Write.
func NewKafkaProducer(cfg KafkaConfig) (*KafkaProducer, error) {
	if len(cfg.Brokers) == 0 {
		return nil, fmt.Errorf("infra: kafka brokers empty")
	}

	return &KafkaProducer{
		w: &kafka.Writer{
			Addr:         kafka.TCP(cfg.Brokers...),
			Balancer:     &kafka.LeastBytes{},
			RequiredAcks: kafka.RequireOne,
			Async:        false,
			BatchTimeout: 100 * time.Millisecond,
			BatchSize:    100,
			WriteTimeout: 10 * time.Second,
			// Note: the Topic field is intentionally left unset. When
			// kafka.Writer.Topic is empty, each kafka.Message must carry its
			// own Topic (this is exactly the topic-agnostic shape we want).
		},
	}, nil
}

// Write synchronously sends one message to the given topic; it returns
// only after the broker acks.
//
// Canceling ctx interrupts Write and returns an error; the caller controls
// the timeout. kafka-go copies key/value internally, so the caller doesn't
// need to retain the slice references.
func (p *KafkaProducer) Write(ctx context.Context, topic string, key, value []byte) error {
	if topic == "" {
		return fmt.Errorf("infra: kafka write: empty topic")
	}

	if err := p.w.WriteMessages(ctx, kafka.Message{
		Topic: topic,
		Key:   key,
		Value: value,
	}); err != nil {
		return fmt.Errorf("infra: kafka write topic=%q: %w", topic, err)
	}

	return nil
}

// Close shuts down the producer and flushes any unsent batches.
func (p *KafkaProducer) Close() error {
	return p.w.Close()
}
