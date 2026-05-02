package usage

import (
	"context"
	"errors"
)

// KafkaWriter KafkaOutbox 对底层 Kafka producer 的依赖。
//
// 接受任何实现"按 (topic, key, value) 同步写入 + 关闭"语义的对象：
//   - 默认：*infra.KafkaProducer（topic-agnostic，每次 Write 指定）
//   - 测试：本包 stub
//   - 用户自定义：可挂 instrumentation / 限流 wrapper
//
// 这样 pkg/usage 不直接依赖 pkg/infra，松耦合 + 可测。
type KafkaWriter interface {
	Write(c context.Context, topic string, key, value []byte) error
	Close() error
}

// KafkaOutbox 把 OutboxEvent 通过 Kafka producer 发到指定 topic。
//
// topic 在构造时绑定（每个 outbox 实例对应一个业务用途的 topic）；
// 同一个 KafkaProducer 可以被多个 outbox（usage / audit / ...）共享。
//
// 实现 OutboxPublisher + io.Closer。
type KafkaOutbox struct {
	w     KafkaWriter
	topic string
}

// NewKafkaOutbox 用现成 KafkaWriter + topic 构造。
//
// 典型用法：usage.NewKafkaOutbox(infra.NewKafkaProducer(brokers), "ai-gateway.usage")
func NewKafkaOutbox(w KafkaWriter, topic string) *KafkaOutbox {
	return &KafkaOutbox{w: w, topic: topic}
}

// Publish 实现 OutboxPublisher.Publish。
//
// evt.Payload 直接作为 Kafka message value（已序列化好的 JSON / Protobuf）；
// evt.Key 作为 partition key（默认 EndpointID，让同 endpoint 的事件落同一 partition）。
func (o *KafkaOutbox) Publish(ctx context.Context, evt *OutboxEvent) error {
	if evt == nil {
		return errors.New("usage: KafkaOutbox.Publish: nil event")
	}
	if o.topic == "" {
		return errors.New("usage: KafkaOutbox: empty topic (must be set at NewKafkaOutbox)")
	}
	return o.w.Write(ctx, o.topic, []byte(evt.Key), evt.Payload)
}

// Close 关闭底层 producer。
//
// 注意：如果一个 KafkaProducer 被多个 outbox 共享，调用方需要自己协调
// 谁负责 Close（避免重复关闭），通常由 main 直接持有 producer 引用统一关。
func (o *KafkaOutbox) Close() error {
	return o.w.Close()
}

// 编译期断言。
var _ OutboxPublisher = (*KafkaOutbox)(nil)
