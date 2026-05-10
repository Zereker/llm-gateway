// Package gemini 是 Google Gemini 协议的 Adapter。
//
// init() 注册到 adapter registry，vendor 名 "gemini"。
//
// 支持两条 auth 路径（adapter 内按 ep.Auth.Type 自动选）：
//   - AI Studio：auth.type = "gemini-key"，公共 API key（x-goog-api-key 头）
//     URL: https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent
//   - Vertex AI：auth.type = "vertex-adc"（用 ADC）或 "oauth2-sa"（嵌入 SA JSON）
//     URL: https://{location}-aiplatform.googleapis.com/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:generateContent
//
// **客户端格式**：客户端按 OpenAI ChatCompletion 格式发请求；adapter 内部翻译成
// Gemini 格式发上游，响应再翻回 OpenAI 格式。客户端无感切 vendor。
//
// **v0.5 不支持**：
//   - Streaming（Gemini 用 :streamGenerateContent + 不同 chunk 格式，单独迭代）
//   - Function calling / tool_use
//   - Vision / multimodal（parts 只支持 text）
//
// 想接入时在 cmd/gateway/main.go 加 blank import：
//
//	import _ "github.com/zereker/llm-gateway/pkg/adapter/gemini"
package gemini

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/adapter"
	"github.com/zereker/llm-gateway/pkg/domain"
)

// Factory 实现 adapter.Factory。
type Factory struct{}

// Metadata 返回静态元信息。
//
// **NativeProtocol = ProtoGemini**：M7 据此找 (envelope.SourceProtocol → ProtoGemini)
// 的 Translator，例如 openai_gemini（客户端用 OpenAI 协议时）。
func (Factory) Metadata() adapter.Metadata {
	return adapter.Metadata{
		Vendor:              "gemini",
		NativeProtocol:      domain.ProtoGemini,
		SupportedModalities: []domain.Modality{domain.ModalityChat},
	}
}

// NewSession 为本次请求构造 Session。
//
// envelope 在 slim adapter 模型里不需要——translator 已经吃 raw body 翻译完。
// 保留参数为了 adapter.Factory 接口兼容；session 不存它。
func (Factory) NewSession(c context.Context, ep *domain.Endpoint, _ *domain.RequestEnvelope) (adapter.Session, error) {
	tp, err := newTokenProvider(c, ep.Auth)
	if err != nil {
		return nil, err
	}
	return newSession(c, ep, tp), nil
}

func init() {
	adapter.Register("gemini", Factory{})
}
