// Package outbox 定义计量事件发布通道：本地日志 + Kafka。
//
// M10 Tracing middleware 通过 Publisher 发送 Usage 事件；
// 内置实现：file (JSONL append) 和 kafka (sync ack=1)。
//
// 详见 docs/architecture/05-metering-billing.md 第 6 节（同步两阶段：本地日志 + Kafka）。
package outbox

import "context"

// Publisher M10 Tracing 发计量事件的依赖接口。
type Publisher interface {
	Publish(c context.Context, evt *Event) error
}

// Event 一条计量事件。
type Event struct {
	Payload []byte // 序列化的 JSON / Protobuf
	Key     string // 分区键（默认 EndpointID）
}
