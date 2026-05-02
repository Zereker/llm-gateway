package usage

import "context"

// EventBus 是 M10 Tracing 发计量事件的传输通道。
//
// 默认实现（详见 docs/architecture/06）：
//   - file:    本地 JSONL append（开发 / 单机）
//   - kafka:   生产推荐
//   - memory:  仅用于测试
type EventBus interface {
	Publish(ctx context.Context, evt *Event) error
}

// Event 一条计量事件。
type Event struct {
	Payload []byte // 已序列化的 JSON / Protobuf
	Key     string // 分区键（默认 EndpointID）
}
