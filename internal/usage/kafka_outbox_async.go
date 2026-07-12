package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zereker/llm-gateway/internal/metric"
)

// AsyncKafkaOutbox is a production-grade Kafka outbox: async + retry + DLQ + metrics.
//
// **Differences from KafkaOutbox**:
//   - Publish is synchronous but doesn't block the main path — the event is
//     enqueued into a buffered channel and returns immediately
//   - a worker goroutine consumes the channel, with N retries + exponential backoff
//   - once retries are exhausted, writes to a DLQ topic (if configured, otherwise drops the event)
//   - exposes metrics: queue depth / publish success rate / DLQ count
//
// **Limitation**: Publish **blocks** when the channel is full — this is
// intentional (to let backpressure propagate upstream). Unconsumed events
// left in the channel are lost on process crash; if strong data integrity
// is required, configure a DLQ topic + a separate sidecar/cron replay, or
// use KafkaOutbox in synchronous mode.
//
// **Lifecycle**: a worker goroutine starts running immediately after
// construction; Close waits for the channel to drain (with a timeout).
//
// Concurrent-safe (channel + atomic counter; the inner KafkaWriter is
// responsible for its own thread-safety).
type AsyncKafkaOutbox struct {
	inner       KafkaWriter
	topic       string
	dlqTopic    string
	maxRetries  int
	backoffBase time.Duration
	queue       chan *OutboxEvent
	logger      *slog.Logger

	// stats
	dropped atomic.Int64 // count of events truly lost when writing to the DLQ also fails

	// shutdown
	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// AsyncOptions configures the options for assembling an AsyncKafkaOutbox.
type AsyncOptions struct {
	// BufferSize is the channel buffer size; 0 = default 1024.
	BufferSize int
	// MaxRetries is the max number of retries per event; 0 = default 3.
	MaxRetries int
	// BackoffBase is the starting duration for exponential backoff; 0 =
	// default 200ms. The Nth retry waits BackoffBase * 2^(N-1).
	BackoffBase time.Duration
	// DLQTopic is the dead-letter queue topic written to once retries are
	// exhausted; empty = drop directly.
	DLQTopic string
	// Logger writes error logs; nil = slog.Default.
	Logger *slog.Logger
}

// NewAsyncKafkaOutbox constructs and starts the worker goroutine.
func NewAsyncKafkaOutbox(w KafkaWriter, topic string, opts AsyncOptions) *AsyncKafkaOutbox {
	if opts.BufferSize <= 0 {
		opts.BufferSize = 1024
	}

	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 3
	}

	if opts.BackoffBase <= 0 {
		opts.BackoffBase = 200 * time.Millisecond
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	o := &AsyncKafkaOutbox{
		inner:       w,
		topic:       topic,
		dlqTopic:    opts.DLQTopic,
		maxRetries:  opts.MaxRetries,
		backoffBase: opts.BackoffBase,
		queue:       make(chan *OutboxEvent, opts.BufferSize),
		logger:      opts.Logger,
		done:        make(chan struct{}),
	}

	o.wg.Add(1)
	go o.worker()

	return o
}

// Publish implements OutboxPublisher.Publish; returns as soon as the event
// is enqueued into the channel.
//
// If the channel is full, this blocks as long as ctx isn't Done; once ctx is
// Done, it returns ctx.Err. It does **not** guarantee the event is
// eventually sent out — only that it was successfully enqueued.
func (o *AsyncKafkaOutbox) Publish(ctx context.Context, evt *OutboxEvent) error {
	if evt == nil {
		return errors.New("usage: AsyncKafkaOutbox.Publish: nil event")
	}
	// Prioritize returning an error if already closed: the three-way select
	// below could **randomly** pick the queue-send case when the queue has
	// room (Go's select picks randomly among multiple ready cases), which
	// would let an event get enqueued even after Close — violating the
	// contract that "Publish rejects after Close". So do a non-blocking
	// closed check up front to guard against that.
	select {
	case <-o.done:
		return errors.New("usage: AsyncKafkaOutbox: closed")
	default:
	}

	select {
	case o.queue <- evt:
		metric.Gauge(metric.OutboxBufferSize, float64(len(o.queue)))
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-o.done:
		return errors.New("usage: AsyncKafkaOutbox: closed")
	}
}

// worker consumes the channel; retry + DLQ.
//
// **Does not rely on close(queue) to exit**: Close only closes done. Once
// done is received, it enters a drain loop, sending out whatever events are
// already in the buffer (non-blocking receive, returns once empty). queue is
// never closed — a concurrent Publish sending on an already-closed channel
// would panic (select picks randomly among multiple ready cases, so relying
// on the done case alone can't prevent that).
func (o *AsyncKafkaOutbox) worker() {
	defer o.wg.Done()

	for {
		select {
		case evt := <-o.queue:
			o.publishOne(evt)
		case <-o.done:
			// drain: exit after emptying the buffer. A tiny number of
			// events that race in against Close may land after drain — they
			// are dropped (async is best-effort by design anyway), no panic.
			for {
				select {
				case evt := <-o.queue:
					o.publishOne(evt)
				default:
					return
				}
			}
		}
	}
}

// publishOne retries a single event; once exhausted, sends to DLQ; if DLQ
// also fails → increments dropped + logs.
func (o *AsyncKafkaOutbox) publishOne(evt *OutboxEvent) {
	delay := o.backoffBase
	for attempt := 0; attempt <= o.maxRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		writeStart := time.Now()
		err := o.inner.Write(ctx, o.topic, []byte(evt.Key), evt.Payload)

		cancel()
		// docs/08 §3: outbox_publish_duration_seconds（labels: driver, result）
		result := "ok"
		if err != nil {
			result = "error"
		}

		metric.Observe(metric.OutboxPublishDurationSeconds, time.Since(writeStart).Seconds(),
			"driver", "async_kafka", "result", result)

		if err == nil {
			metric.Inc(metric.UsagePublishTotal, "backend", "async_kafka", "result", "ok")
			return
		}

		if attempt < o.maxRetries {
			metric.Inc(metric.UsagePublishTotal, "backend", "async_kafka", "result", "retry")
			time.Sleep(delay)
			delay *= 2

			continue
		}
		// retries exhausted
		o.logger.Warn("usage outbox: publish exhausted retries",
			"topic", o.topic, "key", evt.Key, "err", err)
		metric.Inc(metric.UsagePublishTotal, "backend", "async_kafka", "result", "exhausted")
		o.toDLQ(evt, err)

		return
	}
}

// toDLQ writes to the DLQ topic; if that also fails → increments dropped.
func (o *AsyncKafkaOutbox) toDLQ(evt *OutboxEvent, originalErr error) {
	if o.dlqTopic == "" {
		o.dropped.Add(1)
		metric.Inc(metric.OutboxDroppedTotal, "driver", "async_kafka", "reason", "no_dlq")

		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// DLQ payload: the original payload wrapped in an envelope noting the reason
	dlqPayload := []byte(fmt.Sprintf(`{"original_topic":%q,"reason":%q,"payload":%s}`,
		o.topic, originalErr.Error(), string(evt.Payload)))
	if err := o.inner.Write(ctx, o.dlqTopic, []byte(evt.Key), dlqPayload); err != nil {
		o.dropped.Add(1)
		metric.Inc(metric.OutboxDroppedTotal, "driver", "async_kafka", "reason", "dlq_failed")
		metric.Inc(metric.OutboxDLQTotal, "driver", "async_kafka", "result", "error")
		o.logger.Error("usage outbox: DLQ also failed; event lost",
			"dlq_topic", o.dlqTopic, "key", evt.Key, "err", err)

		return
	}
	// docs/08 §3: outbox_dlq_total{driver,result=ok}
	metric.Inc(metric.OutboxDLQTotal, "driver", "async_kafka", "result", "ok")
	metric.Inc(metric.UsagePublishTotal, "backend", "async_kafka", "result", "dlq")
}

// Dropped returns the cumulative count of truly lost events (retries
// exhausted + DLQ also failed). For metrics / troubleshooting only.
func (o *AsyncKafkaOutbox) Dropped() int64 {
	return o.dropped.Load()
}

// Close signals the worker to exit and waits for drain (with a timeout to
// prevent blocking forever).
//
// Only close(done) is called, **never close(queue)** — on the
// shutdown-timeout path a handler goroutine may still be in Publish, and
// sending on an already-closed channel would panic (select picks randomly
// among multiple ready cases, so the done case alone can't prevent that
// race). queue is left for the GC.
//
// After this is called, Publish rejects new events (the done case returns a
// "closed" err). This does **not** Close the inner KafkaWriter — that is
// closed centrally by whoever assembled it in cmd, via their held reference.
func (o *AsyncKafkaOutbox) Close() error {
	o.closeOnce.Do(func() {
		close(o.done)
		// give the worker 30s to fully drain the buffer (drain logic lives in worker's done branch)
		drainDone := make(chan struct{})
		go func() {
			o.wg.Wait()
			close(drainDone)
		}()

		select {
		case <-drainDone:
		case <-time.After(30 * time.Second):
			o.logger.Warn("usage outbox: drain timed out; events in buffer lost",
				"buffer_depth", len(o.queue))
		}
	})

	return nil
}

// Compile-time assertion.
var _ OutboxPublisher = (*AsyncKafkaOutbox)(nil)
