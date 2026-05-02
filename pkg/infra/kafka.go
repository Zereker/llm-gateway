package infra

import (
	"context"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
)

// KafkaProducer 是 Kafka 生产者的薄包装；用于把 usage / 审计等事件发到下游 topic。
//
// 设计取舍（v0.1）：
//   - **同步**（Async=false）：每次 Write 等 broker leader ack，错误立即返回；
//     性能足够（M10 在响应已写出后跑，不阻塞用户感知延迟），调试友好
//   - **ack=1**（RequireOne）：只等 leader 确认，不等所有 replica；耐久性 vs 延迟折中
//   - **LeastBytes balancer**：按 partition 当前队列字节数选，分布均匀；
//     evt.Key 仍可用作 partition key（kafka-go 会优先按 key hash）
//   - **不开 idempotent producer / EOS**：下游消费者负责去重
//
// 调用方负责 Close（一般在 cmd/X/main.go 的 cleanup 链里）。
type KafkaProducer struct {
	w *kafka.Writer
}

// NewKafkaProducer 构造一个连到指定 brokers + topic 的同步 producer。
//
// brokers 是 "host:port" 列表；topic 在构造时绑死，每个 producer 只发一个 topic。
// 不做 ping（kafka.Writer 是 lazy connect）；第一次 Write 才会真正建连，
// 失败时 Write 返回错误。
func NewKafkaProducer(brokers []string, topic string) (*KafkaProducer, error) {
	if len(brokers) == 0 {
		return nil, fmt.Errorf("infra: kafka brokers empty")
	}
	if topic == "" {
		return nil, fmt.Errorf("infra: kafka topic empty")
	}
	return &KafkaProducer{
		w: &kafka.Writer{
			Addr:         kafka.TCP(brokers...),
			Topic:        topic,
			Balancer:     &kafka.LeastBytes{},
			RequiredAcks: kafka.RequireOne,
			Async:        false,
			BatchTimeout: 100 * time.Millisecond,
			BatchSize:    100,
			WriteTimeout: 10 * time.Second,
		},
	}, nil
}

// Write 同步发送一条消息；等 broker ack 才返回。
//
// ctx 取消会中断 Write 并返回错误；调用方控制超时。
// kafka-go 内部会拷贝 key/value，调用方不需要保留 slice 引用。
func (p *KafkaProducer) Write(ctx context.Context, key, value []byte) error {
	if err := p.w.WriteMessages(ctx, kafka.Message{Key: key, Value: value}); err != nil {
		return fmt.Errorf("infra: kafka write: %w", err)
	}
	return nil
}

// Close 关闭 producer 并 flush 未发送的批次。
func (p *KafkaProducer) Close() error {
	return p.w.Close()
}
