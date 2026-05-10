// Package openai 是 OpenAI 协议（chat completions）的 Adapter 实现。
//
// init() 注册到 adapter registry，vendor 名 "openai"。
// 想接入 OpenAI 时在 cmd/gateway/main.go 加 blank import：
//
//	import _ "github.com/zereker/llm-gateway/pkg/adapter/openai"
//
// 用作 OpenAI-compatible 上游（Azure / DeepSeek / vLLM-OpenAI / Ollama）也直接复用本 Adapter，
// 只要 Endpoint.URL 指向各自的 /v1/chat/completions 路径。
package openai

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/adapter"
	"github.com/zereker/llm-gateway/pkg/domain"
)

// Factory 实现 adapter.Factory。
type Factory struct{}

// Metadata 返回静态元信息。
func (Factory) Metadata() adapter.Metadata {
	return adapter.Metadata{
		Vendor:              "openai",
		NativeProtocol:      domain.ProtoOpenAI,
		SupportedModalities: []domain.Modality{
			domain.ModalityChat,
			domain.ModalityEmbedding,
			domain.ModalityImage, // /v1/images/generations 等；admin 配 endpoint 时 routing.url 指向 image API
		},
	}
}

// NewSession 为本次请求构造 Session。
func (Factory) NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (adapter.Session, error) {
	return newSession(c, ep, env), nil
}

func init() {
	adapter.Register("openai", Factory{})
}
