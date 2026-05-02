// Package adapter 定义协议转换层的核心抽象：Adapter / ResponseSession / Translator。
//
// 一个 Adapter 一个上游厂商；同一个 Adapter 可声明多个 Modality。
// 详见 docs/architecture/02-protocol-translation.md。
package adapter

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/zereker-labs/ai-gateway/internal/envelope"
)

// Adapter 单个上游厂商的接入实现。
//
// 每次请求由 Factory 构造一个新实例（不复用、无状态污染）。
type Adapter interface {
	// 元信息
	Vendor() string                              // "openai" / "anthropic" / "vllm" / ...
	NativeProtocol() envelope.SourceProtocol     // 该厂商上游使用的协议族
	SupportedModalities() []envelope.Modality

	// 请求侧
	Init(ctx context.Context, ep EndpointConfig, env *envelope.Envelope) error
	BuildURL() (string, error)
	BuildHeaders(req *http.Request) error
	TransformRequest() ([]byte, error)

	// 响应侧
	NewResponseSession() ResponseSession
}

// EndpointConfig Adapter.Init 的输入。
//
// 不直接传 *scheduling.Endpoint，避免 adapter → scheduling 的反向依赖；
// dispatch 层（scheduling.Executor）负责字段拷贝。
type EndpointConfig struct {
	ID     string
	Vendor string
	URL    string
	APIKey string
	Extra  json.RawMessage
}
