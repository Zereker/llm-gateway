package usage

import "context"

// OutboxPublisher M10 Tracing 发计量事件的依赖接口。
//
// 内置实现：file (JSONL append) 和 kafka (sync ack=1)。
//
// 详见 docs/architecture/05-metering-billing.md 第 6 节（同步两阶段：本地日志 + Kafka）。
type OutboxPublisher interface {
	Publish(c context.Context, evt *OutboxEvent) error
}

// OutboxEvent 一条计量事件。
type OutboxEvent struct {
	Payload []byte // 序列化的 JSON / Protobuf
	Key     string // 分区键（默认 EndpointID）
}
