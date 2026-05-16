package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zereker/llm-gateway/pkg/metric"
)

// AsyncKafkaOutbox 生产级 Kafka outbox：异步 + 重试 + DLQ + metric。
//
// **跟 KafkaOutbox 的区别**：
//   - 同步 Publish 不阻塞主路径——事件入 buffered channel 即返
//   - worker goroutine 消费 channel，N 次重试 + 指数退避
//   - 重试耗尽后写 DLQ topic（配置则启用，否则丢事件）
//   - 暴露 metric：queue depth / publish 成功率 / DLQ 计数
//
// **限制**：channel 满时 Publish **会阻塞**——这是有意的（让上游 backpressure）。
// 进程崩溃时 channel 内未消费的事件会丢；如果数据完整性要求高，应配 DLQ topic
// + 单独 sidecar / cron 重放，或者用 KafkaOutbox 走同步模式。
//
// **lifecycle**：构造后立刻有 worker goroutine 跑；Close 等 channel drain（带 timeout）。
//
// Concurrent-safe（channel + atomic counter；inner KafkaWriter 自己保 thread-safe）。
type AsyncKafkaOutbox struct {
	inner       KafkaWriter
	topic       string
	dlqTopic    string
	maxRetries  int
	backoffBase time.Duration
	queue       chan *OutboxEvent
	logger      *slog.Logger

	// stats
	dropped atomic.Int64 // 写 DLQ 也失败时的真丢数

	// shutdown
	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// AsyncOptions 装配 AsyncKafkaOutbox 的选项。
type AsyncOptions struct {
	// BufferSize channel buffer 大小；0 = 默认 1024。
	BufferSize int
	// MaxRetries 单事件最多 retry 次数；0 = 默认 3。
	MaxRetries int
	// BackoffBase 指数退避起始时长；0 = 默认 200ms。第 N 次 retry 等 BackoffBase * 2^(N-1)。
	BackoffBase time.Duration
	// DLQTopic 重试耗尽后写的 dead-letter queue topic；空 = 直接丢。
	DLQTopic string
	// Logger 写错误日志；nil = slog.Default。
	Logger *slog.Logger
}

// NewAsyncKafkaOutbox 构造 + 启动 worker goroutine。
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

// Publish 实现 OutboxPublisher.Publish；事件入 channel 即返。
//
// channel 满时 ctx 一直未 Done 会阻塞；ctx Done 时返 ctx.Err。
// **不**保证事件最终被发出去——只保证入队成功。
func (o *AsyncKafkaOutbox) Publish(ctx context.Context, evt *OutboxEvent) error {
	if evt == nil {
		return errors.New("usage: AsyncKafkaOutbox.Publish: nil event")
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

// worker 消费 channel；retry + DLQ。
func (o *AsyncKafkaOutbox) worker() {
	defer o.wg.Done()
	for evt := range o.queue {
		o.publishOne(evt)
	}
}

// publishOne retry 单条事件；耗尽后 DLQ；DLQ 也失败 → 增 dropped + log。
func (o *AsyncKafkaOutbox) publishOne(evt *OutboxEvent) {
	delay := o.backoffBase
	for attempt := 0; attempt <= o.maxRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := o.inner.Write(ctx, o.topic, []byte(evt.Key), evt.Payload)
		cancel()
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
		// 重试耗尽
		o.logger.Warn("usage outbox: publish exhausted retries",
			"topic", o.topic, "key", evt.Key, "err", err)
		metric.Inc(metric.UsagePublishTotal, "backend", "async_kafka", "result", "exhausted")
		o.toDLQ(evt, err)
		return
	}
}

// toDLQ 写 DLQ topic；DLQ 也失败 → 增 dropped。
func (o *AsyncKafkaOutbox) toDLQ(evt *OutboxEvent, originalErr error) {
	if o.dlqTopic == "" {
		o.dropped.Add(1)
		metric.Inc(metric.OutboxDroppedTotal, "driver", "async_kafka", "reason", "no_dlq")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// DLQ payload：原 payload 包一层 envelope 标注原因
	dlqPayload := []byte(fmt.Sprintf(`{"original_topic":%q,"reason":%q,"payload":%s}`,
		o.topic, originalErr.Error(), string(evt.Payload)))
	if err := o.inner.Write(ctx, o.dlqTopic, []byte(evt.Key), dlqPayload); err != nil {
		o.dropped.Add(1)
		metric.Inc(metric.OutboxDroppedTotal, "driver", "async_kafka", "reason", "dlq_failed")
		o.logger.Error("usage outbox: DLQ also failed; event lost",
			"dlq_topic", o.dlqTopic, "key", evt.Key, "err", err)
		return
	}
	metric.Inc(metric.UsagePublishTotal, "backend", "async_kafka", "result", "dlq")
}

// Dropped 累计真丢失的事件数（重试耗尽 + DLQ 也失败）。仅 metric / 排障用。
func (o *AsyncKafkaOutbox) Dropped() int64 {
	return o.dropped.Load()
}

// Close 关 channel + 等 worker drain（带 timeout 防止永久阻塞）。
//
// 调用后 Publish 会拒绝新事件（返 "closed" err）。
// **不** Close 内部 KafkaWriter——那个由 cmd 装配方持有引用统一关闭。
func (o *AsyncKafkaOutbox) Close() error {
	o.closeOnce.Do(func() {
		close(o.done)
		close(o.queue)
		// 给 worker 30s 把 buffer drain 完
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

// 编译期断言。
var _ OutboxPublisher = (*AsyncKafkaOutbox)(nil)
