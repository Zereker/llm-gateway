package usage

import "context"

// OutboxPublisher is the dependency interface M10 Tracing uses to emit metering events.
//
// Built-in implementations: file (JSONL append) and kafka (sync ack=1).
//
// See docs/architecture/05-metering-billing.md section 6 for details
// (synchronous two-phase: local log + Kafka).
//
// Implementations MUST be safe for concurrent use (multiple gin handler
// goroutines calling Publish at the same time).
// evt.Payload []byte: implementations must not retain a reference to the
// slice (the caller may reuse it / it may be GC'd).
type OutboxPublisher interface {
	Publish(c context.Context, evt *OutboxEvent) error
}

// OutboxEvent is a single metering event.
type OutboxEvent struct {
	Payload []byte // serialized JSON / Protobuf
	Key     string // partition key (defaults to EndpointID)
}
