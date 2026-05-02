// Package store 定义带 Watch 的 KV 存储抽象：
// 业务侧用它存"运行期可变的、需推送变更"的数据
// （如 ModelService 配置 / RateLimit 阈值 / SchedulerProfile / 端点列表 等）。
//
// 默认实现见 file.go（目录 + 文件）；生产可挂 etcd / sqlite / 其他 KV 后端。
package store

import (
	"context"
	"encoding/json"
)

// KV 带 Watch 的 KV 存储抽象。
//
// Implementations MUST be safe for concurrent Get / List / Put / Delete from
// multiple goroutines. Watch 返回的 channel 由单一 consumer 读取；多个 Watch
// 调用应返回独立 channel。
type KV interface {
	// Get 读单个 key
	Get(c context.Context, key string) (json.RawMessage, error)

	// List 读 prefix 下所有 (key, value)
	List(c context.Context, prefix string) (map[string]json.RawMessage, error)

	// Watch 订阅 prefix 下的变更事件（增 / 改 / 删）
	Watch(c context.Context, prefix string) (<-chan Event, error)

	// Put 写入；Admin API 用
	Put(c context.Context, key string, value json.RawMessage) error

	// Delete 删除；Admin API 用
	Delete(c context.Context, key string) error
}

// Event 变更事件。
type Event struct {
	Type  EventType
	Key   string
	Value json.RawMessage
}

type EventType int

const (
	EventPut EventType = iota
	EventDelete
)
