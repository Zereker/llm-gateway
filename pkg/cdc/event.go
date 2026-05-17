// Package cdc 实现 gateway 端消费 Debezium Server 推到 Redis Streams 的变更事件，
// 维护 L1 LRU + L2 Redis + L3 MySQL fallback 的三层缓存。
//
// 链路：
//
//	admin → MySQL binlog → Debezium Server → Redis Stream (key=llm_gateway.<schema>.<table>)
//	                                                ↓
//	                                    gateway StreamConsumer XREAD
//	                                                ↓
//	                                    invalidate L1 LRU + 更新 L2 Redis cache key
//
// **核心结构**：
//   - Event：Debezium envelope 解析后的 typed 事件（op / before / after / table）
//   - StreamConsumer：阻塞 XREAD 多个 stream key；每条事件触发 EventHandler
//   - TieredCache[T]：L1 LRU + L2 Redis Get + L3 loader fallback
package cdc

import (
	"encoding/json"
	"fmt"
)

// Op Debezium 操作类型。
//
// 来自 Debezium envelope 的 payload.op 字段。
type Op string

const (
	OpRead   Op = "r" // snapshot read（首次启动全量）
	OpCreate Op = "c" // INSERT
	OpUpdate Op = "u" // UPDATE
	OpDelete Op = "d" // DELETE
	// truncate "t" 暂不处理
)

// Event Debezium envelope 解析后的事件。
type Event struct {
	Op     Op              `json:"op"`
	TsMs   int64           `json:"ts_ms"`
	Source EventSource     `json:"source"`
	Before json.RawMessage `json:"before"`
	After  json.RawMessage `json:"after"`
}

// EventSource Debezium source 元信息。
type EventSource struct {
	DB    string `json:"db"`
	Table string `json:"table"`
	TsMs  int64  `json:"ts_ms"`
}

// envelope Debezium Server JSON 形态（无 schema 段；application.properties 关了 schemas.enable）。
//
// 实际 Debezium 默认会包成 {"schema":..., "payload":{op,ts_ms,source,before,after}}；
// 关 schemas.enable 后是 flat 形态。本类型涵盖两种。
type envelope struct {
	Schema  json.RawMessage `json:"schema"`
	Payload Event           `json:"payload"`
	// flat 形态没有外层 schema/payload，字段直接放 envelope 顶层：
	OpFlat     Op              `json:"op,omitempty"`
	TsMsFlat   int64           `json:"ts_ms,omitempty"`
	SourceFlat EventSource     `json:"source,omitempty"`
	BeforeFlat json.RawMessage `json:"before,omitempty"`
	AfterFlat  json.RawMessage `json:"after,omitempty"`
}

// ParseEvent 解析单条 Debezium 消息成 Event。
//
// 兼容两种 shape：
//   - 带 schema：{"schema":..., "payload":{op,...,after,...}}
//   - 不带 schema：{op,ts_ms,source,before,after}（推荐；application.properties 关 schemas.enable）
func ParseEvent(raw []byte) (*Event, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("cdc: empty event")
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("cdc: parse envelope: %w", err)
	}
	// 检测形态：有 payload.op 就用 nested；否则 fallback flat
	if env.Payload.Op != "" {
		return &env.Payload, nil
	}
	if env.OpFlat == "" {
		return nil, fmt.Errorf("cdc: missing op field")
	}
	return &Event{
		Op:     env.OpFlat,
		TsMs:   env.TsMsFlat,
		Source: env.SourceFlat,
		Before: env.BeforeFlat,
		After:  env.AfterFlat,
	}, nil
}

// PrimaryRow 返回 after（c/u/r）或 before（d）；统一拿到"当前关心的那一行"。
func (e *Event) PrimaryRow() json.RawMessage {
	switch e.Op {
	case OpDelete:
		return e.Before
	default:
		return e.After
	}
}

// IsDelete 是否删除事件。
func (e *Event) IsDelete() bool { return e.Op == OpDelete }
