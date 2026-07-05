// Package azureopenai 是 Azure OpenAI 的 vendor 实现。
//
// Azure OpenAI 的**线协议就是 OpenAI**（同样的请求/响应 shape），所以 endpoint 的
// `protocol` 仍填 `openai`——复用 OpenAI 的 translator + response handler + 错误分类
// （embed openai.Factory 继承 Classify）。区别只在 HTTP 层：
//
//	鉴权头：`api-key: <key>`（不是 Authorization: Bearer；那是 Azure AD token 的走法）
//	URL：   带 `?api-version=<ver>` query（deployer 在 routing.url 填完整 Azure 端点，
//	        或填 base + routing.api_version，本 adapter 补 api-version）
//
// 接入方式：deployer 写 endpoint 时 `vendor: azure-openai` + `protocol: openai` +
// `auth.type: bearer`（payload.api_key = Azure key）。cmd/gateway blank import 本包。
package azureopenai

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/protocol/openai"
)

// Factory embed openai.Factory 继承 Classify（Azure 返回 OpenAI 形状的错误 JSON），
// 只覆盖 Metadata（vendor 名）+ NewSession（Azure HTTP 层）。
type Factory struct {
	openai.Factory
}

// Metadata 覆盖 vendor 名。
func (Factory) Metadata() protocol.Metadata {
	return protocol.Metadata{
		Vendor: "azure-openai",
		SupportedModalities: []domain.Modality{
			domain.ModalityChat,
			domain.ModalityEmbedding,
		},
	}
}

// NewSession 用 Azure session（api-key 头 + api-version URL）。
func (Factory) NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (protocol.Session, error) {
	return newSession(c, ep, env), nil
}

func init() {
	protocol.RegisterFactory("azure-openai", Factory{})
}
