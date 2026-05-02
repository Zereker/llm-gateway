package infra

import (
	"context"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
)

// KafkaConfig Kafka producer 连接配置。
//
// pkg/config 通过引用本类型把字段暴露给 yaml；具体出现在 outbox.kafka.brokers 等位置。
// 未来扩展（TLS / SASL / ClientID 等）只在本类型加字段，不动 pkg/config 结构。
type KafkaConfig struct {
	Brokers []string `yaml:"brokers"`
	// Future: TLS, SASL, ClientID, ...
}

// KafkaProducer 是 Kafka 生产者的薄包装；topic-agnostic，调用方按消息指定 topic。
//
// 设计取舍（v0.1）：
//   - **同步**（Async=false）：每次 Write 等 broker leader ack，错误立即返回；
//     性能足够（M10 在响应已写出后跑，不阻塞用户感知延迟），调试友好
//   - **ack=1**（RequireOne）：只等 leader 确认，不等所有 replica；耐久性 vs 延迟折中
//   - **LeastBytes balancer**：按 partition 当前队列字节数选，分布均匀；
//     evt.Key 可用作 partition key（kafka-go 优先按 key hash）
//   - **不开 idempotent producer / EOS**：下游消费者负责去重
//   - **topic 不绑死在 producer 上**：一份 producer 可发任意 topic，业务方
//     （usage outbox / 未来 audit outbox / ...）自己持有自己的 topic 字符串
//
// 调用方负责 Close（一般在 cmd/X/main.go 的 cleanup 链里）。
type KafkaProducer struct {
	w *kafka.Writer
}

// NewKafkaProducer 构造一个连到 cfg.Brokers 的同步 producer。
//
// topic 不在此绑定——每次 Write 由调用方指定。
// 不做 ping（kafka.Writer 是 lazy connect）；第一次 Write 才真正建连。
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
			// 注意：故意不设 Topic 字段。kafka.Writer.Topic 留空时，
			// 每条 kafka.Message 必须自带 Topic（这正是我们想要的 topic-agnostic 形态）。
		},
	}, nil
}

// Write 同步发送一条消息到指定 topic；等 broker ack 才返回。
//
// ctx 取消会中断 Write 并返回错误；调用方控制超时。
// kafka-go 内部会拷贝 key/value，调用方不需要保留 slice 引用。
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

// Close 关闭 producer 并 flush 未发送的批次。
func (p *KafkaProducer) Close() error {
	return p.w.Close()
}
