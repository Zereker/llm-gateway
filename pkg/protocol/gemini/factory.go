// Package gemini 是 Google Gemini 协议的 vendor Factory 实现。
//
// init() 注册到 protocol vendor registry，vendor 名 "gemini"。
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
// **支持**：
//   - Chat（system/user/assistant/text content）
//   - Streaming（客户端 stream:true → :streamGenerateContent?alt=sse，SSE chunk
//     翻成 OpenAI SSE，见 openai_gemini responseHandler）
//
// **不支持**：
//   - Function calling / tool_use
//   - Vision / multimodal（parts 只支持 text）
//
// 想接入时在 cmd/gateway/main.go 加 blank import：
//
//	import _ "github.com/zereker/llm-gateway/pkg/protocol/gemini"
package gemini

import (
	"context"

	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/domain"
)

// Factory 实现 protocol.Factory。
type Factory struct{}

// Metadata 返回静态元信息。endpoint.Protocol（deployer 配置）通常 = ProtoGemini，
// 客户端用 OpenAI SDK 时 dispatcher 自动接入 openai_gemini 翻译。
func (Factory) Metadata() protocol.Metadata {
	return protocol.Metadata{
		Vendor:              "gemini",
		SupportedModalities: []domain.Modality{domain.ModalityChat},
	}
}

// NewSession 为本次请求构造 Session。
//
// 从 envelope 的**原始客户端 body** 读 stream 标志决定走流式端点——翻译后的 Gemini
// body 不带 stream，所以这里从翻译前的 RawBytes 判定（OpenAI/Anthropic/Responses 都
// 用 stream:true，gjson 读 bool 协议无关）。
func (Factory) NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (protocol.Session, error) {
	tp, err := newTokenProvider(c, ep.Auth)
	if err != nil {
		return nil, err
	}
	streaming := env != nil && gjson.GetBytes(env.RawBytes, "stream").Bool()
	return newSession(c, ep, tp, streaming), nil
}

func init() {
	protocol.RegisterFactory("gemini", Factory{})
}
