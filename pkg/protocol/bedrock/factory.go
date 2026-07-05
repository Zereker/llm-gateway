// Package bedrock 是 AWS Bedrock 的 vendor 实现（Anthropic Claude on Bedrock，
// 支持非流式 InvokeModel + 流式 InvokeModelWithResponseStream）。
//
// **线协议 = Anthropic Messages**：endpoint 的 `protocol` 填 `anthropic`，复用
// Anthropic 的 translator + response handler（Bedrock 的 Claude 返回体就是 Anthropic
// Messages JSON）。Bedrock 特有的只在 HTTP 层：
//
//	鉴权：AWS SigV4（service=bedrock），凭证走 AWSSigV4Auth（access/secret/region）。
//	URL： deployer 在 routing.url 填完整 invoke 端点，含 modelId + region host，如
//	      https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20240620-v1:0/invoke
//	body：Anthropic Messages 去掉顶层 model（modelId 在 URL 里）、补 anthropic_version。
//
// **SigV4 用官方 aws-sdk-go-v2 signer**：SigV4 的 canonical-URI 编码（Bedrock 路径带
// `:`）等边界极易手写出错且无法离线对真实端点验证，用官方 signer 保正确。
//
// **流式**：客户端 stream:true 时用 InvokeModelWithResponseStream 端点，响应是 AWS
// event-stream 二进制分帧——由 Factory.DecodeTransport（protocol.TransportDecoder，
// 见 stream.go）解成 Anthropic SSE,再交 openai_anthropic handler 翻成 OpenAI SSE。
// 传输层(解帧)与协议层(shape 翻译)分离,复用现成 Anthropic 流式翻译。
//
// 接入方式：deployer 写 endpoint `vendor: bedrock` + `protocol: anthropic` +
// `auth.type: aws-sigv4`。cmd/gateway blank import 本包。
package bedrock

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
)

// Factory 实现 protocol.Factory。无自定义 Classify——Bedrock 错误是 AWS 形状，
// 走 DefaultClassifier 的 status-based 分类兜底即可。
type Factory struct{}

// Metadata 静态元信息。
func (Factory) Metadata() protocol.Metadata {
	return protocol.Metadata{
		Vendor:              "bedrock",
		SupportedModalities: []domain.Modality{domain.ModalityChat},
	}
}

// NewSession 为本次请求构造 Bedrock session。
func (Factory) NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (protocol.Session, error) {
	return newSession(c, ep, env), nil
}

func init() {
	protocol.RegisterFactory("bedrock", Factory{})
}
