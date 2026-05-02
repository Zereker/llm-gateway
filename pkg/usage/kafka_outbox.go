package usage

import (
	"context"
	"errors"
)

// KafkaWriter KafkaOutbox 对底层 Kafka producer 的依赖。
//
// 接受任何实现"按 key/value 同步写入 + 关闭"语义的对象：
//   - 默认：*infra.KafkaProducer
//   - 测试：本包的 stub
//   - 用户自定义：可挂 instrumentation / 限流 wrapper
//
// 这样 pkg/usage 不直接依赖 pkg/infra，松耦合 + 可测。
type KafkaWriter interface {
	Write(c context.Context, key, value []byte) error
	Close() error
}

// KafkaOutbox 把 OutboxEvent 通过 Kafka producer 发到下游 topic。
//
// 实现 OutboxPublisher + io.Closer。
type KafkaOutbox struct {
	w KafkaWriter
}

// NewKafkaOutbox 用现成 KafkaWriter 构造。
//
// 典型用法：usage.NewKafkaOutbox(infra.NewKafkaProducer(brokers, topic))
func NewKafkaOutbox(w KafkaWriter) *KafkaOutbox {
	return &KafkaOutbox{w: w}
}

// Publish 实现 OutboxPublisher.Publish。
//
// evt.Payload 直接作为 Kafka message value（已经是序列化好的 JSON / Protobuf）；
// evt.Key 作为 partition key（默认 EndpointID，让同 endpoint 的事件落同一 partition，
// 便于消费者本地聚合）。
func (o *KafkaOutbox) Publish(ctx context.Context, evt *OutboxEvent) error {
	if evt == nil {
		return errors.New("usage: KafkaOutbox.Publish: nil event")
	}
	return o.w.Write(ctx, []byte(evt.Key), evt.Payload)
}

// Close 关闭底层 producer。
func (o *KafkaOutbox) Close() error {
	return o.w.Close()
}

// 编译期断言。
var _ OutboxPublisher = (*KafkaOutbox)(nil)
