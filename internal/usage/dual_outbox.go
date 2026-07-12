package usage

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/zereker/llm-gateway/internal/metric"
)

// DualWriteOutbox writes the same OutboxEvent to two sinks:
//
//   - file (sync, source of truth) — a successful write counts as commit
//   - kafka (async best-effort) — provides low-latency broadcast to
//     billing/reconciliation/quota consumers
//
// This is an implementation of the Transactional Outbox Pattern: file is the
// source of truth, Kafka is a mirror. If the broker goes down, commits still
// succeed; landing an event does not depend on Kafka being healthy. An
// external replay tool later reads the file and re-sends any missing events
// to Kafka (consumers dedupe idempotently by event_id).
//
// Comparison with AsyncKafkaOutbox + DLQ:
//
//   - AsyncKafkaOutbox + DLQ: on main-topic failure, writes go to a DLQ
//     topic instead; but the DLQ shares the same broker cluster as the main
//     topic, so if the whole broker goes down, the DLQ fails too.
//   - DualWriteOutbox: file lives on local disk, so the broker failure
//     domain is independent of the disk failure domain; data is only lost
//     if the disk on the gateway process's own machine is full or damaged.
//
// See docs/architecture/05-metering-billing.md §5 (usage outbox) for details.
type DualWriteOutbox struct {
	file  OutboxPublisher
	kafka OutboxPublisher
	log   *slog.Logger
}

// NewDualWriteOutbox composes ready-made file + kafka publishers.
//
// The caller is responsible for constructing each sub-publisher (typically:
// FileOutbox + AsyncKafkaOutbox). Close only closes the file handle; the
// kafka producer's lifecycle is managed centrally by internal/server and is not
// closed by this type — avoiding a double-close.
//
// If log == nil, slog.Default() is used.
func NewDualWriteOutbox(file, kafka OutboxPublisher, log *slog.Logger) *DualWriteOutbox {
	if log == nil {
		log = slog.Default()
	}

	return &DualWriteOutbox{file: file, kafka: kafka, log: log}
}

// Publish implements OutboxPublisher.Publish.
//
// Flow:
//  1. file.Publish (sync, blocking) — durability commit
//  2. kafka.Publish (async, best-effort) — failure does not affect the return value
//
// **file ok + kafka fails**: returns nil (the event has already landed;
// the replay tool backfills the kafka failure). Only a warn log +
// outbox_kafka_publish_error metric are recorded.
//
// **file fails: kafka is not sent, and the error is returned directly.**
// This upholds the "file ⊇ kafka" invariant — any event that shows up in
// kafka must also exist in file, otherwise consumer-vs-file reconciliation
// cannot distinguish a "kafka phantom event" from "file data loss", and file
// stops being the source of truth.
// (The old behavior of "still sending to kafka as a last resort even when
// file fails" broke exactly this invariant; if active-active disaster
// recovery is needed, switch explicitly to the AsyncKafkaOutbox+DLQ mode
// instead of silently inverting the trust relationship.)
func (d *DualWriteOutbox) Publish(ctx context.Context, evt *OutboxEvent) error {
	if evt == nil {
		return errors.New("usage: DualWriteOutbox.Publish: nil event")
	}

	if fileErr := d.file.Publish(ctx, evt); fileErr != nil {
		metric.Inc(metric.OutboxFileErrorTotal, "result", "error")
		d.log.ErrorContext(ctx, "usage_events: file sink publish failed; event NOT forwarded to kafka (file is source of truth)",
			"event_key", evt.Key, "err", fileErr.Error())

		return fileErr
	}

	if err := d.kafka.Publish(ctx, evt); err != nil {
		metric.Inc(metric.OutboxKafkaPublishErrorTotal, "result", "error")
		d.log.WarnContext(ctx, "usage_events: kafka sink publish failed; file has source of truth",
			"event_key", evt.Key, "err", err.Error())
	}

	return nil
}

// Close closes the file sink; the kafka producer is managed centrally by srv.
func (d *DualWriteOutbox) Close() error {
	if c, ok := d.file.(io.Closer); ok {
		return c.Close()
	}

	return nil
}

var (
	_ OutboxPublisher = (*DualWriteOutbox)(nil)
	_ io.Closer       = (*DualWriteOutbox)(nil)
)
