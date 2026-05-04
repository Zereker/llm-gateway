// Package anthropic 是 Anthropic Messages 协议的 Adapter。
//
// init() 注册到 adapter registry，vendor 名 "anthropic"。
//
// **Auth**：anthropic 用 `x-api-key` 头（不是 Authorization Bearer）。schema 用
// AuthTypeXAPIKey。配合公司账号或 Bedrock 转发等多种方式都靠 x-api-key。
//
// **Required header**：`anthropic-version: 2023-06-01`（API 版本）；adapter 自动加。
//
// **客户端格式**：客户端按 OpenAI ChatCompletion 格式发请求；openai_anthropic
// translator 翻译成 Anthropic Messages 格式发上游，响应再翻回 OpenAI。
//
// **v0.5 不支持**：
//   - Streaming（Anthropic SSE 跟 OpenAI 不同；单独迭代）
//   - Function calling / tool_use
//   - Vision / multi-block content（content 数组只取 text）
//
// 想接入时在 cmd/gateway/main.go 加 blank import：
//
//	import _ "github.com/zereker-labs/ai-gateway/pkg/adapter/anthropic"
package anthropic

import (
	"context"

	"github.com/zereker-labs/ai-gateway/pkg/adapter"
	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// Factory 实现 adapter.Factory。
type Factory struct{}

// Metadata 返回静态元信息。
//
// **NativeProtocol = ProtoAnthropic**：M7 据此找 (envelope.SourceProtocol → ProtoAnthropic)
// 的 Translator，例如 openai_anthropic（客户端用 OpenAI 协议时）。
func (Factory) Metadata() adapter.Metadata {
	return adapter.Metadata{
		Vendor:              "anthropic",
		NativeProtocol:      domain.ProtoAnthropic,
		SupportedModalities: []domain.Modality{domain.ModalityChat},
	}
}

// NewSession 为本次请求构造 Session。
func (Factory) NewSession(c context.Context, ep *domain.Endpoint, _ *domain.RequestEnvelope) (adapter.Session, error) {
	return newSession(c, ep), nil
}

func init() {
	adapter.Register("anthropic", Factory{})
}
