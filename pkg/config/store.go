// Package config 定义 ConfigStore 接口（业务配置的实时分发）。
//
// 默认实现：file (fsnotify) / etcd / sqlite，详见 docs/architecture/06。
package config

import (
	"context"
	"encoding/json"
)

// Store 配置中心抽象。
//
// Implementations MUST be safe for concurrent Get / List / Put / Delete from
// multiple goroutines. Watch 返回的 channel 由单一 consumer 读取；多个 Watch
// 调用应返回独立 channel。
type Store interface {
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

// Event 配置变更事件。
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
